package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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

const dataDir = "data"

var logsDir = filepath.Join(dataDir, "logs")

// ==================== 数据结构 ====================

type Config struct {
	SpeedLimitMbps  int      `json:"speed_limit_mbps"`
	DailyQuotaMinGB int      `json:"daily_quota_min_gb"`
	DailyQuotaMaxGB int      `json:"daily_quota_max_gb"`
	ScheduleStart   string   `json:"schedule_start"`
	ScheduleEnd     string   `json:"schedule_end"`
	SleepMinMinutes int      `json:"sleep_min_minutes"`
	SleepMaxMinutes int      `json:"sleep_max_minutes"`
	URLs            []string `json:"urls"`
}

type Stats struct {
	Daily        map[string]uint64 `json:"daily"`
	TodayBytes   uint64            `json:"today_bytes"`
	TodayDate    string            `json:"today_date"`
	TodayQuotaGB int               `json:"today_quota_gb"`
}

type LogEntry struct {
	Time string `json:"time"`
	Msg  string `json:"msg"`
}

type App struct {
	mu sync.Mutex

	config Config
	stats  Stats

	// 日志：按天存储，内存中只保留今天的
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
}

// ==================== 运行调优 ====================

func setLowPriority() {
	syscall.Setpriority(syscall.PRIO_PROCESS, 0, 19)
}

// ==================== 持久化：配置 ====================

func (app *App) loadConfig() {
	data, err := os.ReadFile(filepath.Join(dataDir, "config.json"))
	if err != nil {
		app.config = Config{
			SpeedLimitMbps: 5, DailyQuotaMinGB: 150, DailyQuotaMaxGB: 200,
			ScheduleStart: "00:00", ScheduleEnd: "23:59",
			SleepMinMinutes: 10, SleepMaxMinutes: 20,
			URLs: []string{"http://updates-http.cdn-apple.com/2019WinterFCS/fullrestores/041-39257/32129B6C-292C-11E9-9E72-4511412B0A59/iPhone_4.7_12.1.4_16D57_Restore.ipsw"},
		}
		app.saveConfig()
		return
	}
	json.Unmarshal(data, &app.config)
	app.fixConfig()
}

func (app *App) fixConfig() {
	if app.config.SpeedLimitMbps <= 0 { app.config.SpeedLimitMbps = 5 }
	if app.config.DailyQuotaMinGB <= 0 { app.config.DailyQuotaMinGB = 150 }
	if app.config.DailyQuotaMaxGB <= 0 { app.config.DailyQuotaMaxGB = 200 }
	if app.config.DailyQuotaMaxGB < app.config.DailyQuotaMinGB { app.config.DailyQuotaMaxGB = app.config.DailyQuotaMinGB }
	if app.config.SleepMinMinutes <= 0 { app.config.SleepMinMinutes = 10 }
	if app.config.SleepMaxMinutes <= 0 { app.config.SleepMaxMinutes = 20 }
	if app.config.SleepMaxMinutes < app.config.SleepMinMinutes { app.config.SleepMaxMinutes = app.config.SleepMinMinutes }
	app.currentSpeedLimit.Store(int64(app.config.SpeedLimitMbps))
}

func (app *App) saveConfig() {
	data, _ := json.MarshalIndent(app.config, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "config.json"), data, 0644)
}

// ==================== 持久化：流量统计 ====================

func (app *App) loadStats() {
	data, err := os.ReadFile(filepath.Join(dataDir, "stats.json"))
	if err != nil {
		app.stats = Stats{Daily: make(map[string]uint64), TodayDate: time.Now().Format("2006-01-02")}
		return
	}
	json.Unmarshal(data, &app.stats)
	if app.stats.Daily == nil { app.stats.Daily = make(map[string]uint64) }
}

func (app *App) saveStats() {
	data, _ := json.MarshalIndent(app.stats, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "stats.json"), data, 0644)
}

// ==================== 持久化：按天日志 ====================

func (app *App) logFilePath(date string) string {
	return filepath.Join(logsDir, "log-"+date+".json")
}

func (app *App) loadLogsFromFile(date string) []LogEntry {
	data, err := os.ReadFile(app.logFilePath(date))
	if err != nil {
		return nil
	}
	var result struct {
		Entries []LogEntry `json:"entries"`
	}
	json.Unmarshal(data, &result)
	return result.Entries
}

func (app *App) saveTodayLogs() {
	if len(app.todayLogs) == 0 {
		return
	}
	data, _ := json.MarshalIndent(map[string]interface{}{
		"entries": app.todayLogs,
	}, "", "  ")
	os.WriteFile(app.logFilePath(app.todayLogsDate), data, 0644)
}

func (app *App) addLog(msg string) {
	app.todayLogs = append(app.todayLogs, LogEntry{
		Time: time.Now().Format("15:04:05"),
		Msg:  msg,
	})
	// 单日上限 1000 条
	if len(app.todayLogs) > 1000 {
		app.todayLogs = app.todayLogs[len(app.todayLogs)-1000:]
	}
}

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
	for d := range dateSet {
		dates = append(dates, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	return dates
}

func (app *App) cleanOldLogs() {
	cutoff := time.Now().AddDate(0, 0, -7).Format("2006-01-02")
	files, err := os.ReadDir(logsDir)
	if err != nil {
		return
	}
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

// ==================== 随机额度 ====================

func (app *App) rollTodayQuota() {
	min := app.config.DailyQuotaMinGB
	max := app.config.DailyQuotaMaxGB
	if max <= min {
		app.stats.TodayQuotaGB = min
	} else {
		app.stats.TodayQuotaGB = rand.Intn(max-min+1) + min
	}
	app.addLog(fmt.Sprintf("今日流量限额已生成: %d GB", app.stats.TodayQuotaGB))
}

// ==================== 日期与调度 ====================

func (app *App) checkDateChange() {
	today := time.Now().Format("2006-01-02")
	if app.stats.TodayDate == today {
		return
	}
	// 保存昨天的日志到文件
	app.saveTodayLogs()
	// 归档昨天的流量
	if app.stats.TodayDate != "" && app.stats.TodayBytes > 0 {
		app.stats.Daily[app.stats.TodayDate] = app.stats.TodayBytes
	}
	// 重置
	app.stats.TodayBytes = 0
	app.stats.TodayDate = today
	app.todayLogs = []LogEntry{}
	app.todayLogsDate = today
	app.addLog("日期更新，流量计数器已重置")
	app.rollTodayQuota()
	// 清理旧日志
	app.cleanOldLogs()
	// 年度清理流量数据
	thisYear := time.Now().Format("2006")
	for k := range app.stats.Daily {
		if !strings.HasPrefix(k, thisYear) {
			delete(app.stats.Daily, k)
		}
	}
}

func parseTimeStr(s string) int {
	parts := strings.Split(s, ":")
	if len(parts) != 2 { return 0 }
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	return h*60 + m
}

func (app *App) isInSchedule() bool {
	nowMin := time.Now().Hour()*60 + time.Now().Minute()
	start := parseTimeStr(app.config.ScheduleStart)
	end := parseTimeStr(app.config.ScheduleEnd)
	if start <= end { return nowMin >= start && nowMin <= end }
	return nowMin >= start || nowMin <= end
}

func (app *App) isQuotaReached() bool {
	if app.stats.TodayQuotaGB <= 0 { return false }
	return app.stats.TodayBytes >= uint64(app.stats.TodayQuotaGB)*1_000_000_000
}

func (app *App) sleepWithCheck(seconds int) {
	for i := 0; i < seconds; i++ {
		if !app.isRunning.Load() { return }
		time.Sleep(time.Second)
	}
}

// ==================== 下载引擎 ====================

var userAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/122.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/122.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:123.0) Gecko/20100101 Firefox/123.0",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/122.0.0.0 Safari/537.36",
}

func (app *App) downloadWorker() {
	for {
		if !app.isRunning.Load() { time.Sleep(time.Second); continue }

		app.mu.Lock()
		if !app.isInSchedule() {
			app.status = "out_of_schedule"; app.shouldStop.Store(true)
			app.mu.Unlock(); app.sleepWithCheck(30); continue
		}
		if app.isQuotaReached() {
			app.status = "quota_reached"; app.shouldStop.Store(true)
			app.mu.Unlock(); app.sleepWithCheck(60); continue
		}
		urls := make([]string, len(app.config.URLs)); copy(urls, app.config.URLs)
		sleepMin := app.config.SleepMinMinutes; sleepMax := app.config.SleepMaxMinutes
		app.mu.Unlock()

		if len(urls) == 0 {
			app.mu.Lock(); app.addLog("没有配置下载地址，服务已停止")
			app.isRunning.Store(false); app.status = "stopped"; app.shouldStop.Store(true)
			app.mu.Unlock(); continue
		}

		url := urls[rand.Intn(len(urls))]
		app.mu.Lock(); app.status = "running"; app.shouldStop.Store(false)
		app.addLog("开始下载: " + url); app.mu.Unlock()

		downloaded, err := app.doDownload(url)

		app.mu.Lock()
		if err != nil { app.addLog(fmt.Sprintf("下载异常: %v (已传输 %s)", err, formatBytes(downloaded)))
		} else { app.addLog(fmt.Sprintf("下载完成: %s", formatBytes(downloaded))) }
		app.mu.Unlock()

		if !app.isRunning.Load() { continue }

		if sleepMax < sleepMin { sleepMax = sleepMin }
		sleepMinSec := sleepMin * 60; sleepMaxSec := sleepMax * 60
		sleepSec := sleepMinSec
		if sleepMaxSec > sleepMinSec { sleepSec = rand.Intn(sleepMaxSec-sleepMinSec) + sleepMinSec }
		app.mu.Lock(); app.status = "sleeping"
		app.addLog(fmt.Sprintf("休息 %d 分 %d 秒", sleepSec/60, sleepSec%60)); app.mu.Unlock()
		app.sleepWithCheck(sleepSec)
	}
}

func (app *App) doDownload(url string) (uint64, error) {
	client := &http.Client{
		Timeout:   2 * time.Hour,
		Transport: &http.Transport{IdleConnTimeout: 90 * time.Second, DisableKeepAlives: false},
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

// ==================== 后台协程 ====================

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
		if len(app.speedHistory) > 30 { app.speedHistory = app.speedHistory[len(app.speedHistory)-30:] }
		if app.isRunning.Load() {
			if !app.isInSchedule() { app.shouldStop.Store(true); app.status = "out_of_schedule"
			} else if app.isQuotaReached() { app.shouldStop.Store(true); app.status = "quota_reached"
			} else { app.shouldStop.Store(false) }
		}
		app.mu.Unlock()
	}
}

func (app *App) autoSaver() {
	for range time.NewTicker(60 * time.Second).C {
		app.mu.Lock()
		app.saveStats()
		app.saveTodayLogs()
		app.mu.Unlock()
	}
}

// ==================== HTTP API ====================

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

func (app *App) handleToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" { http.Error(w, "", 405); return }
	app.mu.Lock(); defer app.mu.Unlock()
	nowRunning := !app.isRunning.Load(); app.isRunning.Store(nowRunning)
	if nowRunning {
		app.status = "running"; app.shouldStop.Store(false); app.startedAt = time.Now(); app.addLog("服务已启动")
	} else {
		app.status = "stopped"; app.shouldStop.Store(true); app.speedMbps = 0; app.addLog("服务已停止")
	}
	app.saveStats(); app.saveTodayLogs()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"is_running": nowRunning})
}

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

func (app *App) handleLogDates(w http.ResponseWriter, r *http.Request) {
	app.mu.Lock(); defer app.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(app.getLogDates())
}

func (app *App) handleLogs(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	app.mu.Lock(); defer app.mu.Unlock()
	var entries []LogEntry
	if date == "" || date == app.todayLogsDate {
		entries = app.todayLogs
	} else {
		entries = app.loadLogsFromFile(date)
	}
	if entries == nil { entries = []LogEntry{} }
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"entries": entries})
}

func (app *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var cfg Config
		if json.NewDecoder(r.Body).Decode(&cfg) != nil { http.Error(w, "", 400); return }
		app.mu.Lock()
		app.config = cfg; app.fixConfig(); app.saveConfig()
		app.addLog(fmt.Sprintf("配置已更新: %d Mbps, %d~%d GB/天, %s-%s, 休息 %d~%d 分钟",
			app.config.SpeedLimitMbps, app.config.DailyQuotaMinGB, app.config.DailyQuotaMaxGB,
			app.config.ScheduleStart, app.config.ScheduleEnd,
			app.config.SleepMinMinutes, app.config.SleepMaxMinutes))
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

// ==================== 工具函数 ====================

func formatBytes(b uint64) string {
	switch {
	case b >= 1_000_000_000_000: return fmt.Sprintf("%.2f TB", float64(b)/1e12)
	case b >= 1_000_000_000:     return fmt.Sprintf("%.2f GB", float64(b)/1e9)
	case b >= 1_000_000:         return fmt.Sprintf("%.2f MB", float64(b)/1e6)
	default:                     return fmt.Sprintf("%d B", b)
	}
}

// ==================== 启动入口 ====================

func main() {
	setLowPriority()
	debug.SetGCPercent(200)

	os.MkdirAll(dataDir, 0755)
	os.MkdirAll(logsDir, 0755)

	app := &App{
		status:        "stopped",
		speedHistory:  make([]float64, 30),
		todayLogsDate: time.Now().Format("2006-01-02"),
	}

	app.loadConfig()
	app.loadStats()

	// 尝试从文件恢复今日日志（程序重启后不丢失）
	if entries := app.loadLogsFromFile(app.todayLogsDate); entries != nil {
		app.todayLogs = entries
	}

	if app.stats.TodayQuotaGB <= 0 {
		app.rollTodayQuota()
	}

	app.addLog("DownOnly 初始化完成")
	app.saveTodayLogs()

	go app.downloadWorker()
	go app.speedTracker()
	go app.autoSaver()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" { http.NotFound(w, r); return }
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	http.HandleFunc("/api/status", app.handleStatus)
	http.HandleFunc("/api/toggle", app.handleToggle)
	http.HandleFunc("/api/history", app.handleHistory)
	http.HandleFunc("/api/log_dates", app.handleLogDates)
	http.HandleFunc("/api/logs", app.handleLogs)
	http.HandleFunc("/api/config", app.handleConfig)

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
	http.ListenAndServe("0.0.0.0:"+port, nil)
}