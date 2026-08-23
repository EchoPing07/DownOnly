package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

//go:embed icons
var iconsFS embed.FS

//go:embed vendor/chart.umd.js
var chartJS []byte

const dataDir = "data"

var logsDir = filepath.Join(dataDir, "logs")

var dateRegexp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// errSlowSwitch 哨兵错误：判定器检测到当前源持续低速，通知 doDownload 中断切换
var errSlowSwitch = fmt.Errorf("slow source switch")

// URL 状态常量
const (
	statusNormal  = "normal"
	statusSlow    = "slow"
	statusFailed  = "failed"
	statusUnknown = "unknown"
)

// 私有/保留地址 CIDR（预解析，避免运行时重复计算）
var (
	cidr10         = mustParseCIDR("10.0.0.0/8")
	cidr172        = mustParseCIDR("172.16.0.0/12")
	cidr192        = mustParseCIDR("192.168.0.0/16")
	cidr127        = mustParseCIDR("127.0.0.0/8")
	cidr169        = mustParseCIDR("169.254.0.0/16")
	cidrV6Loopback = mustParseCIDR("::1/128")
	cidrV6Private  = mustParseCIDR("fc00::/7")
	cidrV6Link     = mustParseCIDR("fe80::/10")
	privateCIDRs   = []*net.IPNet{cidr10, cidr172, cidr192, cidr127, cidr169, cidrV6Loopback, cidrV6Private, cidrV6Link}
)

func mustParseCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil { panic("invalid CIDR: " + s) }
	return n
}

// --- 数据结构 ---

// URLEntry 单个下载源条目，Status 随运行时与测试结果流转
//   normal  正常，一等候选
//   slow    运行中判定为低速，永久绕过（不参与慢切换候选），仅可被失败覆盖或手动恢复
//   failed  不可用，选源时跳过
//   unknown 新增/导入未测，等同 normal 参与选源
type URLEntry struct {
	URL      string `json:"url"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
}

// Config 应用配置，持久化至 config.json
type Config struct {
	SpeedLimitMbps       int        `json:"speed_limit_mbps"`
	MinSpeedMbps         int        `json:"min_speed_mbps"`          // 最低速率，0=关闭慢速切换
	SlowSwitchThreshold  int        `json:"slow_switch_threshold"`   // 低速秒数占比阈值 10-90，默认 60
	SlowSwitchMaxAttempts int       `json:"slow_switch_max_attempts"` // 慢切换最大尝试次数，默认 3
	DailyQuotaMinGB      int        `json:"daily_quota_min_gb"`
	DailyQuotaMaxGB      int        `json:"daily_quota_max_gb"`
	ScheduleStart        string     `json:"schedule_start"`
	ScheduleEnd          string     `json:"schedule_end"`
	SleepMinMinutes      int        `json:"sleep_min_minutes"`
	SleepMaxMinutes      int        `json:"sleep_max_minutes"`
	SleepDisabled        bool       `json:"sleep_disabled"`
	URLs                 []URLEntry `json:"urls"`
	AllowPublicAccess    bool       `json:"allow_public_access"`
}

// UnmarshalJSON 兼容旧版 block_public_access 字段与旧版 urls([]string) 格式
func (c *Config) UnmarshalJSON(data []byte) error {
	type Alias Config
	aux := &struct {
		BlockPublicAccess *bool     `json:"block_public_access"`
		URLs              []json.RawMessage `json:"urls"`
		*Alias
	}{Alias: (*Alias)(c)}
	if err := json.Unmarshal(data, aux); err != nil { return err }
	if aux.BlockPublicAccess != nil && !*aux.BlockPublicAccess { c.AllowPublicAccess = true }
	// urls 可能是旧版 []string 或新版 []URLEntry，逐个探测
	if aux.URLs != nil {
		entries := make([]URLEntry, 0, len(aux.URLs))
		for _, raw := range aux.URLs {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				entries = append(entries, URLEntry{URL: s, Status: statusUnknown})
				continue
			}
			var e URLEntry
			if json.Unmarshal(raw, &e) == nil { entries = append(entries, e) }
		}
		c.URLs = entries
	}
	return nil
}

// Stats 流量统计，持久化至 stats.json
type Stats struct {
	Daily        map[string]uint64 `json:"daily"`
	TodayBytes   uint64            `json:"today_bytes"`
	TodayDate    string            `json:"today_date"`
	TodayQuotaGB int               `json:"today_quota_gb"`
	Enabled      bool              `json:"enabled"`
}

// LogEntry 单条日志记录
type LogEntry struct {
	Time string `json:"time"`
	Msg  string `json:"msg"`
}

// App 应用核心结构体，管理配置、统计、日志及下载生命周期
type App struct {
	mu sync.Mutex

	config Config
	stats  Stats

	todayLogs     []LogEntry
	todayLogsDate string

	status       string
	speedMbps    float64
	speedHistory []float64
	startedAt    time.Time

	isRunning         atomic.Bool
	shouldStop        atomic.Bool
	bytesThisSecond   atomic.Uint64
	bytesAccumulator  atomic.Uint64
	currentSpeedLimit atomic.Int64

	// 慢速切换相关（mu 保护，除 shouldSwitch 为 atomic 供 doDownload 高频检测）
	currentURL            string
	currentURLStart       time.Time
	shouldSwitch          atomic.Bool // 判定器置位 → doDownload 中断切源
	sessionSlowTriggered  bool         // 本次下载期间是否触发过慢判定
	noCandidateNotified   bool         // 无候选日志去重
}

// --- 持久化：配置 ---

// defaultConfig 返回出厂默认配置
func defaultConfig() Config {
	return Config{
		SpeedLimitMbps: 5, MinSpeedMbps: 0, SlowSwitchThreshold: 60, SlowSwitchMaxAttempts: 3,
		DailyQuotaMinGB: 150, DailyQuotaMaxGB: 200,
		ScheduleStart: "00:00", ScheduleEnd: "23:59",
		SleepMinMinutes: 10, SleepMaxMinutes: 20,
		AllowPublicAccess: false,
		URLs: []URLEntry{{URL: "http://updates-http.cdn-apple.com/2019WinterFCS/fullrestores/041-39257/32129B6C-292C-11E9-9E72-4511412B0A59/iPhone_4.7_12.1.4_16D57_Restore.ipsw", Status: statusUnknown}},
	}
}

// backupCorruptFile 把无法解析的持久化文件改名备份，避免回退默认值后被覆盖写盘丢失用户数据
func backupCorruptFile(path string) {
	bak := path + ".corrupt-" + time.Now().Format("20060102-150405")
	if err := os.Rename(path, bak); err != nil {
		log.Printf("损坏文件备份失败: %v", err)
		return
	}
	log.Printf("已备份损坏文件: %s", bak)
}

// loadConfig 从 config.json 加载配置，文件不存在或损坏时使用默认值（损坏文件先备份）
func (app *App) loadConfig() {
	data, err := os.ReadFile(filepath.Join(dataDir, "config.json"))
	if err == nil {
		if err := json.Unmarshal(data, &app.config); err != nil {
			log.Printf("配置文件解析失败: %v", err)
			backupCorruptFile(filepath.Join(dataDir, "config.json"))
		} else {
			app.fixConfig()
			return
		}
	}
	app.config = defaultConfig()
	app.fixConfig()
	app.saveConfig()
}

// fixConfig 修正非法配置值并同步速率限制到原子变量
func (app *App) fixConfig() {
	if app.config.SpeedLimitMbps <= 0 { app.config.SpeedLimitMbps = 5 }
	if app.config.MinSpeedMbps < 0 { app.config.MinSpeedMbps = 0 }
	if app.config.MinSpeedMbps > app.config.SpeedLimitMbps { app.config.MinSpeedMbps = app.config.SpeedLimitMbps }
	if app.config.SlowSwitchThreshold < 10 { app.config.SlowSwitchThreshold = 60 }
	if app.config.SlowSwitchThreshold > 90 { app.config.SlowSwitchThreshold = 90 }
	if app.config.SlowSwitchMaxAttempts <= 0 { app.config.SlowSwitchMaxAttempts = 3 }
	if app.config.DailyQuotaMinGB <= 0 { app.config.DailyQuotaMinGB = 150 }
	if app.config.DailyQuotaMaxGB <= 0 { app.config.DailyQuotaMaxGB = 200 }
	if app.config.DailyQuotaMaxGB < app.config.DailyQuotaMinGB { app.config.DailyQuotaMaxGB = app.config.DailyQuotaMinGB }
	// sleep 区间允许 0（连续下载），仅修正负值；validateConfig 已保证 min<=max
	if app.config.SleepMinMinutes < 0 { app.config.SleepMinMinutes = 10 }
	if app.config.SleepMaxMinutes < 0 { app.config.SleepMaxMinutes = 20 }
	if app.config.SleepMaxMinutes < app.config.SleepMinMinutes { app.config.SleepMaxMinutes = app.config.SleepMinMinutes }
	// 时间字段兜底：被污染/手改的 config.json 可能带空串或非法时间，
	// 修复到合法值保证 GET /api/config 返回的配置能通过 POST 自身校验（回环自洽），
	// 否则前端整体回传配置（如添加地址）会被 400 拒绝
	if !validateTimeStr(app.config.ScheduleStart) { app.config.ScheduleStart = "00:00" }
	if !validateTimeStr(app.config.ScheduleEnd) { app.config.ScheduleEnd = "23:59" }
	if app.config.ScheduleStart == app.config.ScheduleEnd {
		app.config.ScheduleStart, app.config.ScheduleEnd = "00:00", "23:59"
	}
	for i := range app.config.URLs {
		if app.config.URLs[i].URL == "" { continue }
		s := app.config.URLs[i].Status
		if s != statusNormal && s != statusSlow && s != statusFailed && s != statusUnknown {
			app.config.URLs[i].Status = statusUnknown
		}
		if app.config.URLs[i].Attempts < 0 { app.config.URLs[i].Attempts = 0 }
	}
	app.currentSpeedLimit.Store(int64(app.config.SpeedLimitMbps))
}

// validateTimeStr 校验 HH:MM 格式时间字符串
func validateTimeStr(s string) bool {
	parts := strings.Split(s, ":")
	if len(parts) != 2 { return false }
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 { return false }
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 { return false }
	return true
}

// validateConfig 校验完整配置合法性
func validateConfig(cfg Config) error {
	if cfg.SpeedLimitMbps < 1 || cfg.SpeedLimitMbps > 1000 {
		return fmt.Errorf("速率限制须在 1~1000 Mbps 之间")
	}
	if cfg.MinSpeedMbps < 0 || cfg.MinSpeedMbps > cfg.SpeedLimitMbps {
		return fmt.Errorf("最低速率须在 0 至速率限制之间")
	}
	if cfg.SlowSwitchThreshold < 10 || cfg.SlowSwitchThreshold > 90 {
		return fmt.Errorf("慢速切换阈值须在 10%%~90%% 之间")
	}
	if cfg.SlowSwitchMaxAttempts < 1 || cfg.SlowSwitchMaxAttempts > 99 {
		return fmt.Errorf("慢速切换最大尝试次数须在 1~99 之间")
	}
	if cfg.DailyQuotaMinGB < 1 || cfg.DailyQuotaMinGB > 99999 {
		return fmt.Errorf("每日限额最小值须在 1~99999 GB 之间")
	}
	if cfg.DailyQuotaMaxGB < cfg.DailyQuotaMinGB || cfg.DailyQuotaMaxGB > 99999 {
		return fmt.Errorf("每日限额最大值须大于等于最小值且不超过 99999 GB")
	}
	if !validateTimeStr(cfg.ScheduleStart) {
		return fmt.Errorf("运行开始时间格式不正确，请使用 HH:MM 格式")
	}
	if !validateTimeStr(cfg.ScheduleEnd) {
		return fmt.Errorf("运行结束时间格式不正确，请使用 HH:MM 格式")
	}
	if parseTimeStr(cfg.ScheduleStart) == parseTimeStr(cfg.ScheduleEnd) {
		return fmt.Errorf("运行开始时间与结束时间不能相同")
	}
	if !cfg.SleepDisabled {
		if cfg.SleepMinMinutes < 0 || cfg.SleepMinMinutes > 1440 {
			return fmt.Errorf("休息间隔最小值须在 0~1440 分钟之间")
		}
		if cfg.SleepMaxMinutes < cfg.SleepMinMinutes || cfg.SleepMaxMinutes > 1440 {
			return fmt.Errorf("休息间隔最大值须大于等于最小值且不超过 1440 分钟")
		}
	}
	return nil
}

// writeFileAtomic 先写同目录临时文件再替换，避免断电/磁盘满导致目标文件截断损坏
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil { return err }
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil { tmp.Close(); os.Remove(name); return err }
	if err := tmp.Close(); err != nil { os.Remove(name); return err }
	if err := os.Chmod(name, perm); err != nil { os.Remove(name); return err }
	if err := os.Rename(name, path); err != nil { os.Remove(name); return err }
	return nil
}

// saveConfig 持久化配置到 config.json
func (app *App) saveConfig() {
	data, err := json.MarshalIndent(app.config, "", "  ")
	if err != nil { log.Printf("配置序列化失败: %v", err); return }
	if err := writeFileAtomic(filepath.Join(dataDir, "config.json"), data, 0600); err != nil {
		log.Printf("配置文件写入失败: %v", err)
	}
}

// --- 持久化：流量统计 ---

// loadStats 从 stats.json 加载流量统计，文件损坏时备份后重置
func (app *App) loadStats() {
	data, err := os.ReadFile(filepath.Join(dataDir, "stats.json"))
	if err != nil {
		app.stats = Stats{Daily: make(map[string]uint64), TodayDate: time.Now().Format("2006-01-02")}
		return
	}
	if err := json.Unmarshal(data, &app.stats); err != nil {
		log.Printf("统计数据解析失败: %v", err)
		backupCorruptFile(filepath.Join(dataDir, "stats.json"))
		app.stats = Stats{Daily: make(map[string]uint64), TodayDate: time.Now().Format("2006-01-02")}
		return
	}
	if app.stats.Daily == nil { app.stats.Daily = make(map[string]uint64) }
}

// saveStats 持久化流量统计到 stats.json
func (app *App) saveStats() {
	data, err := json.MarshalIndent(app.stats, "", "  ")
	if err != nil { log.Printf("统计序列化失败: %v", err); return }
	if err := writeFileAtomic(filepath.Join(dataDir, "stats.json"), data, 0600); err != nil {
		log.Printf("统计文件写入失败: %v", err)
	}
}

// --- 持久化：按天日志 ---

// logFilePath 返回指定日期的日志文件路径
func (app *App) logFilePath(date string) string {
	return filepath.Join(logsDir, "log-"+date+".json")
}

// loadLogsFromFile 从指定日期的日志文件加载条目
func (app *App) loadLogsFromFile(date string) []LogEntry {
	data, err := os.ReadFile(app.logFilePath(date))
	if err != nil { return nil }
	var result struct { Entries []LogEntry `json:"entries"` }
	if err := json.Unmarshal(data, &result); err != nil {
		log.Printf("日志文件 %s 解析失败: %v", date, err)
	}
	return result.Entries
}

// saveTodayLogs 持久化当日日志到文件
func (app *App) saveTodayLogs() {
	if len(app.todayLogs) == 0 { return }
	data, err := json.MarshalIndent(map[string]interface{}{"entries": app.todayLogs}, "", "  ")
	if err != nil { log.Printf("日志序列化失败: %v", err); return }
	if err := writeFileAtomic(app.logFilePath(app.todayLogsDate), data, 0600); err != nil {
		log.Printf("日志文件写入失败: %v", err)
	}
}

// addLog 追加日志条目，超出上限时淘汰最早的记录
func (app *App) addLog(msg string) {
	app.todayLogs = append(app.todayLogs, LogEntry{
		Time: time.Now().Format("15:04:05"),
		Msg:  msg,
	})
	if len(app.todayLogs) > 1000 {
		app.todayLogs = app.todayLogs[len(app.todayLogs)-1000:]
	}
}

// getLogDates 获取所有可用日志日期，按降序排列
func (app *App) getLogDates() []string {
	dateSet := make(map[string]bool)
	dateSet[app.todayLogsDate] = true
	files, err := os.ReadDir(logsDir)
	if err == nil {
		for _, f := range files {
			name := f.Name()
			if strings.HasPrefix(name, "log-") && strings.HasSuffix(name, ".json") {
				date := strings.TrimPrefix(strings.TrimSuffix(name, ".json"), "log-")
				dateSet[date] = true
			}
		}
	}
	dates := make([]string, 0, len(dateSet))
	for d := range dateSet { dates = append(dates, d) }
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	return dates
}

// cleanOldLogs 清理超过 7 天的历史日志文件
func (app *App) cleanOldLogs() {
	cutoff := time.Now().AddDate(0, 0, -7).Format("2006-01-02")
	files, err := os.ReadDir(logsDir)
	if err != nil { return }
	for _, f := range files {
		name := f.Name()
		if strings.HasPrefix(name, "log-") && strings.HasSuffix(name, ".json") {
			date := strings.TrimPrefix(strings.TrimSuffix(name, ".json"), "log-")
			if date < cutoff {
				os.Remove(filepath.Join(logsDir, name))
				app.addLog("已清理过期日志: " + date)
			}
		}
	}
}

// --- 随机额度 ---

// rollTodayQuota 在 [min, max] 范围内随机生成今日流量限额
func (app *App) rollTodayQuota() {
	min := app.config.DailyQuotaMinGB
	max := app.config.DailyQuotaMaxGB
	if max <= min { app.stats.TodayQuotaGB = min } else {
		app.stats.TodayQuotaGB = rand.Intn(max-min+1) + min
	}
	app.addLog(fmt.Sprintf("今日流量限额已生成: %d GB", app.stats.TodayQuotaGB))
}

// --- 日期与调度 ---

// checkDateChange 日期变更时重置计数器、归档昨日数据、清理过期日志和统计
func (app *App) checkDateChange() {
	today := time.Now().Format("2006-01-02")
	if app.stats.TodayDate == today { return }
	app.saveTodayLogs()
	if app.stats.TodayDate != "" && app.stats.TodayBytes > 0 {
		app.stats.Daily[app.stats.TodayDate] = app.stats.TodayBytes
	}
	app.stats.TodayBytes = 0
	app.stats.TodayDate = today
	app.todayLogs = []LogEntry{}
	app.todayLogsDate = today
	app.addLog("日期更新，流量计数器已重置")
	app.rollTodayQuota()
	app.cleanOldLogs()
	thisYear := time.Now().Format("2006")
	for k := range app.stats.Daily {
		if !strings.HasPrefix(k, thisYear) { delete(app.stats.Daily, k) }
	}
	// 日期变更：全量重置所有源的慢切换计数器
	for i := range app.config.URLs {
		app.config.URLs[i].Attempts = 0
	}
	app.saveConfig()
}

// parseTimeStr 将 HH:MM 解析为自午夜起的分钟数
func parseTimeStr(s string) int {
	parts := strings.Split(s, ":")
	if len(parts) != 2 { return 0 }
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	return h*60 + m
}

// isInSchedule 判断当前时间是否在配置的运行时段内，支持跨午夜
func (app *App) isInSchedule() bool {
	nowMin := time.Now().Hour()*60 + time.Now().Minute()
	start := parseTimeStr(app.config.ScheduleStart)
	end := parseTimeStr(app.config.ScheduleEnd)
	if start <= end { return nowMin >= start && nowMin <= end }
	return nowMin >= start || nowMin <= end
}

// isQuotaReached 判断今日流量是否已达限额
func (app *App) isQuotaReached() bool {
	if app.stats.TodayQuotaGB <= 0 { return false }
	return app.stats.TodayBytes >= uint64(app.stats.TodayQuotaGB)*1_000_000_000
}

// sleepWithCheck 可中断的休眠，每秒检测 isRunning 状态
func (app *App) sleepWithCheck(seconds int) {
	for i := 0; i < seconds; i++ {
		if !app.isRunning.Load() { return }
		time.Sleep(time.Second)
	}
}

// --- 下载引擎 ---

// userAgents 轮换使用的 User-Agent 列表
var userAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/122.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/122.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:123.0) Gecko/20100101 Firefox/123.0",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/122.0.0.0 Safari/537.36",
}

// pickNextURL 选源。triggeredBySlow=true 表示慢切换触发（候选={normal,unknown}，无候选返回 nil）；
// false 表示 EOF 后新轮次（优先 {normal,unknown}，无则用 slow 兑底，failed 不选）。
func (app *App) pickNextURL(triggeredBySlow bool) *URLEntry {
	var normal, slow []int
	for i := range app.config.URLs {
		u := &app.config.URLs[i]
		if u.URL == "" { continue }
		switch u.Status {
		case statusNormal, statusUnknown:
			normal = append(normal, i)
		case statusSlow:
			slow = append(slow, i)
		}
	}
	rand.Shuffle(len(normal), func(i, j int) { normal[i], normal[j] = normal[j], normal[i] })
	rand.Shuffle(len(slow), func(i, j int) { slow[i], slow[j] = slow[j], slow[i] })
	if len(normal) > 0 { return &app.config.URLs[normal[0]] }
	if triggeredBySlow { return nil } // 慢切换不兑底 slow
	if len(slow) > 0 { return &app.config.URLs[slow[0]] }
	return nil
}

// findURLEntry 返回指定 URL 的条目指针，找不到返回 nil
func (app *App) findURLEntry(rawURL string) *URLEntry {
	for i := range app.config.URLs {
		if app.config.URLs[i].URL == rawURL { return &app.config.URLs[i] }
	}
	return nil
}

// downloadWorker 主下载循环：检查调度/额度 → 选择地址 → 执行下载 → 休眠
func (app *App) downloadWorker() {
	for {
		if !app.isRunning.Load() { time.Sleep(time.Second); continue }

		app.mu.Lock()
		if !app.isInSchedule() {
			app.status = "out_of_schedule"; app.shouldStop.Store(true)
			app.currentURL = ""
			app.mu.Unlock(); app.sleepWithCheck(30); continue
		}
		if app.isQuotaReached() {
			app.status = "quota_reached"; app.shouldStop.Store(true)
			app.currentURL = ""
			app.mu.Unlock(); app.sleepWithCheck(60); continue
		}
		if len(app.config.URLs) == 0 {
			app.addLog("没有配置下载地址，服务已停止")
			app.isRunning.Store(false); app.status = "stopped"; app.shouldStop.Store(true)
			app.stats.Enabled = false; app.saveStats()
			app.mu.Unlock(); continue
		}
		// EOF 后新轮次选源：优先 {normal,unknown}，无则用 slow 兑底，failed 不选
		entry := app.pickNextURL(false)
		if entry == nil {
			app.addLog("没有可用下载地址，服务已停止")
			app.isRunning.Store(false); app.status = "stopped"; app.shouldStop.Store(true)
			app.stats.Enabled = false; app.saveStats()
			app.mu.Unlock(); continue
		}
		sleepMin := app.config.SleepMinMinutes; sleepMax := app.config.SleepMaxMinutes
		sleepDisabled := app.config.SleepDisabled
		urlCount := len(app.config.URLs)
		app.mu.Unlock()

		// 单源时重试一次，多源不重试（由选源轮换覆盖）
		maxTries := 1
		if urlCount == 1 { maxTries = 2 }

		var succeeded bool
		var slowSwitched bool
		for try := 0; try < maxTries; try++ {
			if !app.isRunning.Load() { break }
			u := entry.URL
			allowed, transient := isAllowedURL(u)
			if !allowed {
				app.mu.Lock()
				if transient {
					// DNS 瞬时失败：只跳过本轮，不持久化 failed，避免网络抖动误杀源
					app.addLog("暂时无法解析域名，本轮跳过: " + u)
				} else {
					app.addLog("URL 不安全，已跳过: " + u)
					if e := app.findURLEntry(u); e != nil { e.Status = statusFailed }
					app.saveConfig()
				}
				app.mu.Unlock()
				break
			}

			// 进入下载：重置会话级状态
			app.mu.Lock()
			app.currentURL = u
			app.currentURLStart = time.Now()
			app.shouldSwitch.Store(false)
			app.sessionSlowTriggered = false
			app.noCandidateNotified = false
			app.status = "running"; app.shouldStop.Store(false)
			app.addLog("开始下载: " + u)
			app.mu.Unlock()

			downloaded, err := app.doDownload(u)

			app.mu.Lock()
			curEntry := app.findURLEntry(u)
			switch {
			case errors.Is(err, errSlowSwitch):
				// 状态与 attempts 已由判定器更新，此处仅记录日志
				app.addLog(fmt.Sprintf("低速切换，已传输 %s", formatBytes(downloaded)))
				app.mu.Unlock()
				// 慢切换后立即尝试选下一个源（候选={normal,unknown}）
				app.mu.Lock()
				next := app.pickNextURL(true)
				app.mu.Unlock()
				if next != nil {
					entry = next
					slowSwitched = true
					continue // 慢切换不睡觉，直接试下一个源
				}
				// 无候选：不切，当前源慢跑，退出 for-try
				app.mu.Lock()
				app.addLog("无可用候选源，当前源继续慢跑")
				app.mu.Unlock()
				break
			case err != nil:
				app.addLog(fmt.Sprintf("下载异常: %v (已传输 %s)", err, formatBytes(downloaded)))
				if curEntry != nil { curEntry.Status = statusFailed }
				app.saveConfig()
				app.mu.Unlock()
			default: // EOF 成功
				if app.sessionSlowTriggered {
					// 本次下载期间触发过慢判定且未达标，保持原状态
					app.addLog(fmt.Sprintf("下载完成但低速未恢复: %s", formatBytes(downloaded)))
				} else {
					app.addLog(fmt.Sprintf("下载完成: %s", formatBytes(downloaded)))
					if curEntry != nil { curEntry.Status = statusNormal; curEntry.Attempts = 0 }
					app.saveConfig()
				}
				app.mu.Unlock()
				succeeded = true
			}
			if succeeded || errors.Is(err, errSlowSwitch) { break }
			// 非慢切换的失败：单源重试场景下再试一次
			if app.isRunning.Load() {
				app.mu.Lock(); app.addLog("下载失败，重试该地址"); app.mu.Unlock()
			}
		}

		if !app.isRunning.Load() { continue }
		if slowSwitched { continue } // 慢切换已换源，不睡觉直接下一轮

		// 休息逻辑
		if !succeeded && !sleepDisabled {
			if sleepMax < sleepMin { sleepMax = sleepMin }
			sleepMinSec := sleepMin * 60; sleepMaxSec := sleepMax * 60
			sleepSec := sleepMinSec
			if sleepMaxSec > sleepMinSec { sleepSec = rand.Intn(sleepMaxSec-sleepMinSec) + sleepMinSec }
			app.mu.Lock(); app.status = "sleeping"
			app.addLog(fmt.Sprintf("所有地址均失败，休息 %d 分 %d 秒", sleepSec/60, sleepSec%60)); app.mu.Unlock()
			app.sleepWithCheck(sleepSec)
		} else if succeeded && !sleepDisabled {
			if sleepMax < sleepMin { sleepMax = sleepMin }
			sleepMinSec := sleepMin * 60; sleepMaxSec := sleepMax * 60
			sleepSec := sleepMinSec
			if sleepMaxSec > sleepMinSec { sleepSec = rand.Intn(sleepMaxSec-sleepMinSec) + sleepMinSec }
			app.mu.Lock(); app.status = "sleeping"
			app.addLog(fmt.Sprintf("休息 %d 分 %d 秒", sleepSec/60, sleepSec%60)); app.mu.Unlock()
			app.sleepWithCheck(sleepSec)
		}
	}
}

// doDownload 执行单次 HTTP 下载，按速率限制节流并累计流量。
// 检测到 shouldSwitch（判定器置位）时返回 errSlowSwitch，由 worker 走切换分支。
func (app *App) doDownload(url string) (uint64, error) {
	// 不设 Client.Timeout：它覆盖含读 body 的端到端总时长，大文件 × 低限速（如 3GB @ 2Mbps）
	// 会被整体误杀并把源标 failed。连接/握手/等首包各有超时，body 读取由调度/限额/shouldStop 控制。
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			DisableKeepAlives:     false,
		},
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil { return 0, err }
	req.Header.Set("User-Agent", userAgents[rand.Intn(len(userAgents))])
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := client.Do(req)
	if err != nil { return 0, err }
	defer resp.Body.Close()
	if resp.StatusCode != 200 { return 0, fmt.Errorf("HTTP %d", resp.StatusCode) }

	buf := make([]byte, 256*1024)
	var total uint64; var chunkBytes int64; chunkStart := time.Now()
	limitMbps := app.currentSpeedLimit.Load()
	bytesPerSec := limitMbps * 1_000_000 / 8
	chunkTarget := bytesPerSec / 4
	if chunkTarget < 64*1024 { chunkTarget = 64 * 1024 }

	for {
		if app.shouldStop.Load() || !app.isRunning.Load() { return total, nil }
		if app.shouldSwitch.Load() { return total, errSlowSwitch }
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			total += uint64(n); chunkBytes += int64(n)
			app.bytesAccumulator.Add(uint64(n)); app.bytesThisSecond.Add(uint64(n))
			if chunkBytes >= chunkTarget {
				limitMbps = app.currentSpeedLimit.Load()
				bytesPerSec = limitMbps * 1_000_000 / 8
				chunkTarget = bytesPerSec / 4
				if chunkTarget < 64*1024 { chunkTarget = 64 * 1024 }
				elapsed := time.Since(chunkStart)
				expected := time.Duration(float64(chunkBytes) / float64(bytesPerSec) * float64(time.Second))
				if expected > elapsed { time.Sleep(expected - elapsed) }
				chunkBytes = 0; chunkStart = time.Now()
			}
		}
		if readErr != nil {
			if readErr == io.EOF { return total, nil }
			return total, readErr
		}
	}
}

// --- 后台协程 ---

// speedTracker 每秒更新速率、累计流量、检查调度/额度状态
func (app *App) speedTracker() {
	for range time.NewTicker(time.Second).C {
		delta := app.bytesAccumulator.Swap(0)
		speedBytes := app.bytesThisSecond.Swap(0)
		app.mu.Lock()
		if delta > 0 { app.stats.TodayBytes += delta }
		app.checkDateChange()
		mbps := float64(speedBytes) * 8 / 1e6
		if !app.isRunning.Load() { mbps = 0 }
		app.speedMbps = mbps
		app.speedHistory = append(app.speedHistory, mbps)
		if len(app.speedHistory) > 60 { app.speedHistory = app.speedHistory[len(app.speedHistory)-60:] }
		if app.isRunning.Load() {
			if !app.isInSchedule() { app.shouldStop.Store(true); app.status = "out_of_schedule"
			} else if app.isQuotaReached() { app.shouldStop.Store(true); app.status = "quota_reached"
			} else { app.shouldStop.Store(false) }
		}
		app.mu.Unlock()
	}
}

// autoSaver 每 60 秒定时持久化统计和日志
func (app *App) autoSaver() {
	for range time.NewTicker(60 * time.Second).C {
		app.mu.Lock()
		app.saveStats()
		app.saveTodayLogs()
		app.mu.Unlock()
	}
}

// slowSwitchJudge 每 5 秒检测当前下载源是否持续低速，必要时触发切换。
// 判定逻辑：speedHistory 最近 60 个采样中，低于 MinSpeedMbps 的秒数占比超阈值 → 慢。
// 每个源开始下载后延迟 60s 才参与判定（兼任预热，避免慢启动误判）。
func (app *App) slowSwitchJudge() {
	for range time.NewTicker(5 * time.Second).C {
		app.mu.Lock()
		// 关闭 / 空闲 / 预热未过 → 跳过
		if app.config.MinSpeedMbps <= 0 || !app.isRunning.Load() || app.currentURL == "" || app.currentURLStart.IsZero() {
			app.mu.Unlock(); continue
		}
		if time.Since(app.currentURLStart) < 60*time.Second { app.mu.Unlock(); continue }
		cur := app.currentURL
		minSpeed := app.config.MinSpeedMbps
		threshold := app.config.SlowSwitchThreshold
		maxAttempts := app.config.SlowSwitchMaxAttempts
		hist := make([]float64, len(app.speedHistory))
		copy(hist, app.speedHistory)
		app.mu.Unlock()

		// 统计低速秒数占比
		var lowCount int
		for _, v := range hist {
			if float64(v) < float64(minSpeed) { lowCount++ }
		}
		ratio := 0
		if len(hist) > 0 { ratio = lowCount * 100 / len(hist) }
		if ratio <= threshold { continue } // 不慢

		app.mu.Lock()
		// 标记本次下载期间触发过慢判定
		app.sessionSlowTriggered = true
		curEntry := app.findURLEntry(cur)
		if curEntry == nil { app.mu.Unlock(); continue }

		// 达最大尝试次数：静默不切（达 max 不重复打日志）
		if curEntry.Attempts >= maxAttempts { app.mu.Unlock(); continue }

		// 检查慢切换候选池（排除当前源后的 {normal,unknown}）
		hasCandidate := false
		for i := range app.config.URLs {
			u := &app.config.URLs[i]
			if u.URL == cur { continue }
			if u.Status == statusNormal || u.Status == statusUnknown { hasCandidate = true; break }
		}

		if hasCandidate {
			curEntry.Status = statusSlow
			curEntry.Attempts++
			app.shouldSwitch.Store(true)
			app.addLog(fmt.Sprintf("%s 速率过低（低速占比 %d%%），切换下一个源（第 %d/%d 次）",
				cur, ratio, curEntry.Attempts, maxAttempts))
			app.saveConfig()
			app.mu.Unlock()
		} else {
			// 无候选：当前源标 slow（若还不是），不切，慢跑，日志去重
			if curEntry.Status != statusSlow { curEntry.Status = statusSlow }
			if !app.noCandidateNotified {
				app.noCandidateNotified = true
				app.addLog(fmt.Sprintf("%s 速率过低但无可用候选源，当前源继续慢跑（请检查链接质量或网络环境）", cur))
				app.saveConfig()
			}
			app.mu.Unlock()
		}
	}
}

// --- HTTP API ---

// handleStatus 返回运行状态、速率、历史流量等实时信息
func (app *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	app.mu.Lock(); defer app.mu.Unlock()
	var uptime int64
	if app.isRunning.Load() { uptime = int64(time.Since(app.startedAt).Seconds()) }
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": app.status, "speed_mbps": app.speedMbps, "speed_history": app.speedHistory,
		"today_bytes": app.stats.TodayBytes, "today_date": app.stats.TodayDate,
		"today_quota_gb": app.stats.TodayQuotaGB, "uptime_seconds": uptime,
	})
}

// handleToggle 切换服务启停状态
func (app *App) handleToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" { http.Error(w, "", 405); return }
	app.mu.Lock(); defer app.mu.Unlock()
	nowRunning := !app.isRunning.Load(); app.isRunning.Store(nowRunning)
	if nowRunning {
		app.status = "running"; app.shouldStop.Store(false); app.startedAt = time.Now(); app.addLog("服务已启动")
	} else {
		app.status = "stopped"; app.shouldStop.Store(true); app.speedMbps = 0
		app.currentURL = ""; app.shouldSwitch.Store(false)
		app.addLog("服务已停止")
	}
	app.stats.Enabled = nowRunning
	app.saveStats(); app.saveTodayLogs()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"is_running": nowRunning})
}

// handleHistory 返回指定月份每日流量数据
func (app *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	month, err := strconv.Atoi(r.URL.Query().Get("month"))
	if err != nil || month < 1 || month > 12 { month = int(time.Now().Month()) }
	app.mu.Lock(); defer app.mu.Unlock()
	year := time.Now().Year()
	lastDay := time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.Local).Day()
	var totalBytes uint64
	days := make([]map[string]interface{}, 0, lastDay)
	for d := 1; d <= lastDay; d++ {
		dateStr := fmt.Sprintf("%04d-%02d-%02d", year, month, d)
		var b uint64
		if dateStr == app.stats.TodayDate { b = app.stats.TodayBytes } else { b = app.stats.Daily[dateStr] }
		totalBytes += b; days = append(days, map[string]interface{}{"day": d, "bytes": b})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"month": month, "month_total_bytes": totalBytes, "days": days})
}

// handleLogDates 返回所有可用日志日期列表
func (app *App) handleLogDates(w http.ResponseWriter, r *http.Request) {
	app.mu.Lock(); defer app.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(app.getLogDates())
}

// handleLogs 返回指定日期的日志条目，默认返回当日
func (app *App) handleLogs(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date != "" && !dateRegexp.MatchString(date) {
		http.Error(w, "Invalid date format", http.StatusBadRequest)
		return
	}
	app.mu.Lock(); defer app.mu.Unlock()
	var entries []LogEntry
	if date == "" || date == app.todayLogsDate { entries = app.todayLogs } else { entries = app.loadLogsFromFile(date) }
	if entries == nil { entries = []LogEntry{} }
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"entries": entries})
}

// handleTestURL 探测指定 URL 的可达性（GET + Range 只取 1 字节）。
// 测试结果同步更新 URL 状态：通过则 failed/unknown→normal（slow 不变）；失败则 normal/unknown→failed（slow/failed 不变）。
func (app *App) handleTestURL(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	respondTest := func(ok bool) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": ok})
	}
	if rawURL == "" {
		respondTest(false)
		return
	}
	allowed, transient := isAllowedURL(rawURL)
	if !allowed {
		// DNS 瞬时失败不改持久化状态，避免一次网络抖动把源洗成 failed
		if !transient { app.applyTestResult(rawURL, false) }
		respondTest(false)
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		app.applyTestResult(rawURL, false)
		respondTest(false)
		return
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("User-Agent", userAgents[rand.Intn(len(userAgents))])
	resp, err := client.Do(req)
	if err != nil {
		app.applyTestResult(rawURL, false)
		respondTest(false)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	ok := resp.StatusCode >= 200 && resp.StatusCode < 400
	app.applyTestResult(rawURL, ok)
	respondTest(ok)
}

// applyTestResult 根据测试结果更新单个 URL 的持久化状态
func (app *App) applyTestResult(rawURL string, ok bool) {
	app.mu.Lock()
	defer app.mu.Unlock()
	e := app.findURLEntry(rawURL)
	if e == nil { return }
	if ok {
		if e.Status == statusFailed || e.Status == statusUnknown { e.Status = statusNormal }
		// slow 不被批量测试洗白
	} else {
		if e.Status == statusNormal || e.Status == statusUnknown { e.Status = statusFailed }
		// slow/failed 不变
	}
	app.saveConfig()
}

// handleRestoreURL 手动将指定 URL 恢复为正常（slow/failed→normal，attempts 清零）
func (app *App) handleRestoreURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" { http.Error(w, "", 405); return }
	rawURL := r.URL.Query().Get("url")
	app.mu.Lock(); defer app.mu.Unlock()
	e := app.findURLEntry(rawURL)
	if e == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": false}); return
	}
	e.Status = statusNormal
	e.Attempts = 0
	app.saveConfig()
	app.addLog("手动恢复正常: " + rawURL)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handleConfig GET 返回当前配置，POST 更新配置
func (app *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var cfg Config
		if json.NewDecoder(r.Body).Decode(&cfg) != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "请求格式错误"})
			return
		}
		if err := validateConfig(cfg); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		app.mu.Lock()
		// URL 的 status/attempts 为服务端权威：保留当前服务端状态，仅新增 URL 标 unknown。
		// 避免前端陈旧快照覆盖运行时慢速/失败检测。
		oldEntries := make(map[string]URLEntry, len(app.config.URLs))
		for _, e := range app.config.URLs { oldEntries[e.URL] = e }
		for i := range cfg.URLs {
			if old, ok := oldEntries[cfg.URLs[i].URL]; ok {
				cfg.URLs[i].Status = old.Status
				cfg.URLs[i].Attempts = old.Attempts
			} else {
				cfg.URLs[i].Status = statusUnknown
				cfg.URLs[i].Attempts = 0
			}
		}
		app.config = cfg; app.fixConfig(); app.saveConfig()
		sleepInfo := fmt.Sprintf("休息 %d~%d 分钟", app.config.SleepMinMinutes, app.config.SleepMaxMinutes)
		if app.config.SleepDisabled { sleepInfo = "休息已禁用" }
		slowInfo := "慢速切换关闭"
		if app.config.MinSpeedMbps > 0 {
			slowInfo = fmt.Sprintf("慢速切换 %dMbps/占比 %d%%/最大 %d 次",
				app.config.MinSpeedMbps, app.config.SlowSwitchThreshold, app.config.SlowSwitchMaxAttempts)
		}
		app.addLog(fmt.Sprintf("配置已更新: %d Mbps, %s, %d~%d GB/天, %s-%s, %s, 允许公网: %v",
			app.config.SpeedLimitMbps, slowInfo, app.config.DailyQuotaMinGB, app.config.DailyQuotaMaxGB,
			app.config.ScheduleStart, app.config.ScheduleEnd,
			sleepInfo, app.config.AllowPublicAccess))
		if app.stats.TodayQuotaGB < app.config.DailyQuotaMinGB || app.stats.TodayQuotaGB > app.config.DailyQuotaMaxGB {
			app.rollTodayQuota()
		}
		app.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	app.mu.Lock(); defer app.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(app.config)
}

// --- 安全工具 ---

// isPrivateIP 判断 IP 是否为私有/保留地址（RFC 1918、环回、链路本地、未指定地址等）
func isPrivateIP(ip net.IP) bool {
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	for _, cidr := range privateCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// getRemoteIP 从请求中提取客户端 IP
func getRemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// blockPublicMiddleware 公网访问拦截中间件，未开启公网访问时仅允许私有地址；
// 同时拒绝浏览器跨站请求（Sec-Fetch-Site: cross-site），防止恶意网页借局域网内
// 受害者浏览器 CSRF 修改配置/开启公网访问。非浏览器客户端不带该头，不受影响。
func (app *App) blockPublicMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		app.mu.Lock()
		allow := app.config.AllowPublicAccess
		app.mu.Unlock()
		if !allow {
			ip := net.ParseIP(getRemoteIP(r))
			if ip == nil || !isPrivateIP(ip) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// isAllowedURL 校验下载地址安全性：仅允许公网 HTTP(S)，防止 SSRF 攻击。
// transient=true 表示仅因 DNS 暂时失败无法判定，调用方不应据此持久化 failed 状态。
func isAllowedURL(rawURL string) (ok, transient bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false, false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false, false
	}
	hostname := u.Hostname()
	if hostname == "" {
		return false, false
	}
	if hostname == "localhost" || hostname == "[::1]" {
		return false, false
	}
	ip := net.ParseIP(hostname)
	if ip != nil {
		return !isPrivateIP(ip), false
	}
	// DNS 解析后检查所有 IP，防止 DNS Rebinding 绕过
	ips, err := net.LookupIP(hostname)
	if err != nil { return false, true }
	for _, ip := range ips {
		if isPrivateIP(ip) { return false, false }
	}
	return true, false
}

// --- 工具函数 ---

// formatBytes 字节量格式化为人类可读单位（B/MB/GB/TB）
func formatBytes(b uint64) string {
	switch {
	case b >= 1_000_000_000_000: return fmt.Sprintf("%.2f TB", float64(b)/1e12)
	case b >= 1_000_000_000:     return fmt.Sprintf("%.2f GB", float64(b)/1e9)
	case b >= 1_000_000:         return fmt.Sprintf("%.2f MB", float64(b)/1e6)
	default:                     return fmt.Sprintf("%d B", b)
	}
}

// --- 启动入口 ---

// main 初始化应用、恢复状态、注册路由、启动 HTTP 服务
func main() {
	setLowPriority()
	debug.SetGCPercent(200)

	os.MkdirAll(dataDir, 0755)
	os.MkdirAll(logsDir, 0755)

	app := &App{
		status:        "stopped",
		speedHistory:  make([]float64, 0, 60),
		todayLogsDate: time.Now().Format("2006-01-02"),
	}

	app.loadConfig()
	app.loadStats()

	if entries := app.loadLogsFromFile(app.todayLogsDate); entries != nil {
		app.todayLogs = entries
	}

	if app.stats.TodayQuotaGB <= 0 {
		app.rollTodayQuota()
	}

	// 恢复上次运行状态
	if app.stats.Enabled {
		app.isRunning.Store(true)
		app.shouldStop.Store(false)
		app.status = "running"
		app.startedAt = time.Now()
		app.addLog("程序启动，自动恢复上次运行状态")
	} else {
		app.addLog("DownOnly 初始化完成")
	}

	app.saveTodayLogs()

	go app.downloadWorker()
	go app.speedTracker()
	go app.autoSaver()
	go app.slowSwitchJudge()

	http.HandleFunc("/", app.blockPublicMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" { http.NotFound(w, r); return }
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	}))
	http.HandleFunc("/api/status", app.blockPublicMiddleware(app.handleStatus))
	http.HandleFunc("/api/toggle", app.blockPublicMiddleware(app.handleToggle))
	http.HandleFunc("/api/history", app.blockPublicMiddleware(app.handleHistory))
	http.HandleFunc("/api/log_dates", app.blockPublicMiddleware(app.handleLogDates))
	http.HandleFunc("/api/logs", app.blockPublicMiddleware(app.handleLogs))
	http.HandleFunc("/api/config", app.blockPublicMiddleware(app.handleConfig))
	http.HandleFunc("/api/test_url", app.blockPublicMiddleware(app.handleTestURL))
	http.HandleFunc("/api/restore_url", app.blockPublicMiddleware(app.handleRestoreURL))

	http.HandleFunc("/vendor/chart.umd.js", app.blockPublicMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=604800")
		w.Write(chartJS)
	}))

	http.HandleFunc("/icons/", app.blockPublicMiddleware(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/icons/")
		if name == "" || strings.Contains(name, "..") || strings.Contains(name, "/") {
			http.NotFound(w, r); return
		}
		data, err := iconsFS.ReadFile("icons/" + name)
		if err != nil { http.NotFound(w, r); return }
		ct := "image/svg+xml"
		if strings.HasSuffix(name, ".png") { ct = "image/png" }
		if strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, ".jpeg") { ct = "image/jpeg" }
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(data)
	}))

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		fmt.Println("\n正在保存数据...")
		app.mu.Lock()
		delta := app.bytesAccumulator.Swap(0)
		app.stats.TodayBytes += delta
		app.addLog("收到退出信号，正在保存")
		app.saveStats()
		app.saveTodayLogs()
		app.mu.Unlock()
		os.Exit(0)
	}()

	port := "8080"
	if len(os.Args) > 1 { port = os.Args[1] }
	fmt.Printf("DownOnly 已启动 → http://0.0.0.0:%s\n", port)
	srv := &http.Server{
		Addr:         "0.0.0.0:" + port,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // 下载接口需要长时间写入
		IdleTimeout:  120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}