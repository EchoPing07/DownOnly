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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

const dataDir = "data"

// ==================== 数据结构 ====================

type Config struct {
	SpeedLimitMbps int      `json:"speed_limit_mbps"`
	DailyQuotaGB   int      `json:"daily_quota_gb"`
	ScheduleStart  string   `json:"schedule_start"`
	ScheduleEnd    string   `json:"schedule_end"`
	URLs           []string `json:"urls"`
}

type Stats struct {
	Daily      map[string]uint64 `json:"daily"`
	TodayBytes uint64            `json:"today_bytes"`
	TodayDate  string            `json:"today_date"`
}

type LogEntry struct {
	Time string `json:"time"`
	Msg  string `json:"msg"`
}

type LogStore struct {
	MaxEntries int        `json:"max_entries"`
	Entries    []LogEntry `json:"entries"`
}

type App struct {
	mu sync.Mutex

	config Config
	stats  Stats
	logs   LogStore

	// 运行时状态（仅内存）
	isRunning       bool
	status          string
	speedMbps       float64
	speedHistory    []float64
	startedAt       time.Time
	bytesThisSecond uint64
}

// ==================== 持久化 ====================

func (app *App) loadConfig() {
	data, err := os.ReadFile(filepath.Join(dataDir, "config.json"))
	if err != nil {
		app.config = Config{
			SpeedLimitMbps: 5,
			DailyQuotaGB:   200,
			ScheduleStart:  "00:00",
			ScheduleEnd:    "23:59",
			URLs: []string{
				"http://updates-http.cdn-apple.com/2019WinterFCS/fullrestores/041-39257/32129B6C-292C-11E9-9E72-4511412B0A59/iPhone_4.7_12.1.4_16D57_Restore.ipsw",
			},
		}
		app.saveConfig()
		return
	}
	json.Unmarshal(data, &app.config)
}

func (app *App) saveConfig() {
	data, _ := json.MarshalIndent(app.config, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "config.json"), data, 0644)
}

func (app *App) loadStats() {
	data, err := os.ReadFile(filepath.Join(dataDir, "stats.json"))
	if err != nil {
		app.stats = Stats{
			Daily:     make(map[string]uint64),
			TodayDate: time.Now().Format("2006-01-02"),
		}
		return
	}
	json.Unmarshal(data, &app.stats)
	if app.stats.Daily == nil {
		app.stats.Daily = make(map[string]uint64)
	}
}

func (app *App) saveStats() {
	data, _ := json.MarshalIndent(app.stats, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "stats.json"), data, 0644)
}

func (app *App) loadLogs() {
	data, err := os.ReadFile(filepath.Join(dataDir, "logs.json"))
	if err != nil {
		app.logs = LogStore{MaxEntries: 500, Entries: []LogEntry{}}
		return
	}
	json.Unmarshal(data, &app.logs)
	if app.logs.MaxEntries == 0 {
		app.logs.MaxEntries = 500
	}
}

func (app *App) saveLogs() {
	data, _ := json.MarshalIndent(app.logs, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "logs.json"), data, 0644)
}

func (app *App) addLog(msg string) {
	app.logs.Entries = append(app.logs.Entries, LogEntry{
		Time: time.Now().Format("15:04:05"),
		Msg:  msg,
	})
	if len(app.logs.Entries) > app.logs.MaxEntries {
		app.logs.Entries = app.logs.Entries[len(app.logs.Entries)-app.logs.MaxEntries:]
	}
}

// ==================== 日期与调度 ====================

func (app *App) checkDateChange() {
	today := time.Now().Format("2006-01-02")
	if app.stats.TodayDate == today {
		return
	}
	// 归档昨天的数据
	if app.stats.TodayDate != "" && app.stats.TodayBytes > 0 {
		app.stats.Daily[app.stats.TodayDate] = app.stats.TodayBytes
	}
	app.stats.TodayBytes = 0
	app.stats.TodayDate = today
	app.addLog("日期更新，流量计数器已重置")
	// 年度清理：删除非今年的记录
	thisYear := time.Now().Format("2006")
	for k := range app.stats.Daily {
		if !strings.HasPrefix(k, thisYear) {
			delete(app.stats.Daily, k)
		}
	}
}

func parseTimeStr(s string) int {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0
	}
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	return h*60 + m
}

func (app *App) isInSchedule() bool {
	nowMin := time.Now().Hour()*60 + time.Now().Minute()
	start := parseTimeStr(app.config.ScheduleStart)
	end := parseTimeStr(app.config.ScheduleEnd)
	if start <= end {
		return nowMin >= start && nowMin <= end
	}
	return nowMin >= start || nowMin <= end // 跨午夜
}

func (app *App) isQuotaReached() bool {
	return app.stats.TodayBytes >= uint64(app.config.DailyQuotaGB)*1e9
}

// 可中断的休眠：每秒检查是否需要停止
func (app *App) sleepWithCheck(seconds int) {
	for i := 0; i < seconds; i++ {
		app.mu.Lock()
		running := app.isRunning
		app.mu.Unlock()
		if !running {
			return
		}
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
		// 1. 是否启用？
		app.mu.Lock()
		running := app.isRunning
		app.mu.Unlock()
		if !running {
			time.Sleep(time.Second)
			continue
		}

		// 2. 是否在时间窗口内？
		app.mu.Lock()
		if !app.isInSchedule() {
			app.status = "out_of_schedule"
			app.mu.Unlock()
			app.sleepWithCheck(30)
			continue
		}

		// 3. 是否超出配额？
		if app.isQuotaReached() {
			app.status = "quota_reached"
			app.mu.Unlock()
			app.sleepWithCheck(60)
			continue
		}

		// 4. 取配置快照
		urls := make([]string, len(app.config.URLs))
		copy(urls, app.config.URLs)
		speedLimit := app.config.SpeedLimitMbps
		app.mu.Unlock()

		if len(urls) == 0 {
			app.mu.Lock()
			app.addLog("没有配置下载地址，服务已停止")
			app.isRunning = false
			app.status = "stopped"
			app.mu.Unlock()
			continue
		}

		// 5. 随机选一个地址开始下载
		url := urls[rand.Intn(len(urls))]

		app.mu.Lock()
		app.status = "running"
		app.addLog("开始下载: " + truncateURL(url))
		app.mu.Unlock()

		downloaded, err := app.doDownload(url, speedLimit)

		app.mu.Lock()
		if err != nil {
			app.addLog(fmt.Sprintf("下载异常: %v (已传输 %s)", err, formatBytes(downloaded)))
		} else {
			app.addLog(fmt.Sprintf("下载完成: %s", formatBytes(downloaded)))
		}
		stillRunning := app.isRunning
		app.mu.Unlock()

		if !stillRunning {
			continue
		}

		// 6. 随机休息 10~20 分钟
		sleepSec := rand.Intn(600) + 600
		app.mu.Lock()
		app.status = "sleeping"
		app.addLog(fmt.Sprintf("休息 %d 秒", sleepSec))
		app.mu.Unlock()

		app.sleepWithCheck(sleepSec)
	}
}

func (app *App) doDownload(url string, speedLimitMbps int) (uint64, error) {
	client := &http.Client{Timeout: 60 * time.Minute}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgents[rand.Intn(len(userAgents))])
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Mbps → Bytes/s : 5 Mbps = 5*1000000/8 = 625000 B/s
	bytesPerSec := int64(speedLimitMbps) * 1000000 / 8
	buf := make([]byte, 32*1024) // 32KB 缓冲区，对 512MB 内存友好
	var total uint64
	var windowBytes int64
	windowStart := time.Now()

	for {
		// 检查是否应该停止
		app.mu.Lock()
		running := app.isRunning
		quota := app.isQuotaReached()
		app.mu.Unlock()
		if !running || quota {
			return total, nil
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			total += uint64(n)
			windowBytes += int64(n)

			app.mu.Lock()
			app.stats.TodayBytes += uint64(n)
			app.bytesThisSecond += uint64(n)
			app.mu.Unlock()

			// 精准限速：计算当前窗口应该花费的时间
			elapsed := time.Since(windowStart)
			expected := time.Duration(float64(windowBytes) / float64(bytesPerSec) * float64(time.Second))
			if expected > elapsed {
				time.Sleep(expected - elapsed)
			}

			// 每秒重置窗口
			if time.Since(windowStart) >= time.Second {
				windowBytes = 0
				windowStart = time.Now()
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				return total, nil
			}
			return total, readErr
		}
	}
}

// ==================== 后台协程 ====================

// 每秒更新速度和折线图数据
func (app *App) speedTracker() {
	for range time.NewTicker(time.Second).C {
		app.mu.Lock()
		app.checkDateChange()

		bytes := app.bytesThisSecond
		app.bytesThisSecond = 0

		mbps := float64(bytes) * 8 / 1e6
		if !app.isRunning {
			mbps = 0
		}
		app.speedMbps = mbps

		app.speedHistory = append(app.speedHistory, mbps)
		if len(app.speedHistory) > 30 {
			app.speedHistory = app.speedHistory[len(app.speedHistory)-30:]
		}
		app.mu.Unlock()
	}
}

// 每 60 秒自动保存数据到磁盘
func (app *App) autoSaver() {
	for range time.NewTicker(60 * time.Second).C {
		app.mu.Lock()
		app.saveStats()
		app.saveLogs()
		app.mu.Unlock()
	}
}

// ==================== HTTP API ====================

func (app *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	app.mu.Lock()
	defer app.mu.Unlock()

	var uptime int64
	if app.isRunning {
		uptime = int64(time.Since(app.startedAt).Seconds())
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         app.status,
		"speed_mbps":     app.speedMbps,
		"speed_history":  app.speedHistory,
		"today_bytes":    app.stats.TodayBytes,
		"today_date":     app.stats.TodayDate,
		"uptime_seconds": uptime,
		"daily_quota_gb": app.config.DailyQuotaGB,
	})
}

func (app *App) handleToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "", 405)
		return
	}
	app.mu.Lock()
	defer app.mu.Unlock()

	app.isRunning = !app.isRunning
	if app.isRunning {
		app.status = "running"
		app.startedAt = time.Now()
		app.addLog("服务已启动")
	} else {
		app.status = "stopped"
		app.speedMbps = 0
		app.addLog("服务已停止")
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"is_running": app.isRunning})
}

func (app *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	month, err := strconv.Atoi(r.URL.Query().Get("month"))
	if err != nil || month < 1 || month > 12 {
		month = int(time.Now().Month())
	}

	app.mu.Lock()
	defer app.mu.Unlock()

	year := time.Now().Year()
	lastDay := time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.Local).Day()

	var totalBytes uint64
	days := make([]map[string]interface{}, 0, lastDay)

	for d := 1; d <= lastDay; d++ {
		dateStr := fmt.Sprintf("%04d-%02d-%02d", year, month, d)
		var b uint64
		if dateStr == app.stats.TodayDate {
			b = app.stats.TodayBytes
		} else if v, ok := app.stats.Daily[dateStr]; ok {
			b = v
		}
		totalBytes += b
		days = append(days, map[string]interface{}{"day": d, "bytes": b})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"month":             month,
		"month_total_bytes": totalBytes,
		"days":              days,
	})
}

func (app *App) handleLogs(w http.ResponseWriter, r *http.Request) {
	app.mu.Lock()
	defer app.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(app.logs)
}

func (app *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var cfg Config
		if json.NewDecoder(r.Body).Decode(&cfg) != nil {
			http.Error(w, "", 400)
			return
		}
		app.mu.Lock()
		app.config = cfg
		app.saveConfig()
		app.addLog(fmt.Sprintf("配置已更新: %d Mbps, %d GB/天, %s - %s",
			cfg.SpeedLimitMbps, cfg.DailyQuotaGB, cfg.ScheduleStart, cfg.ScheduleEnd))
		app.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(app.config)
}

// ==================== 工具函数 ====================

func truncateURL(u string) string {
	if len(u) > 60 {
		return u[:57] + "..."
	}
	return u
}

func formatBytes(b uint64) string {
	switch {
	case b >= 1e12:
		return fmt.Sprintf("%.2f TB", float64(b)/1e12)
	case b >= 1e9:
		return fmt.Sprintf("%.2f GB", float64(b)/1e9)
	case b >= 1e6:
		return fmt.Sprintf("%.2f MB", float64(b)/1e6)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// ==================== 启动入口 ====================

func main() {
	os.MkdirAll(dataDir, 0755)

	app := &App{
		status:       "stopped",
		speedHistory: make([]float64, 30),
	}

	// 加载持久化数据
	app.loadConfig()
	app.loadStats()
	app.loadLogs()
	app.addLog("DownOnly 初始化完成")
	app.saveLogs()

	// 启动后台协程
	go app.downloadWorker()
	go app.speedTracker()
	go app.autoSaver()

	// 路由注册
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	http.HandleFunc("/api/status", app.handleStatus)
	http.HandleFunc("/api/toggle", app.handleToggle)
	http.HandleFunc("/api/history", app.handleHistory)
	http.HandleFunc("/api/logs", app.handleLogs)
	http.HandleFunc("/api/config", app.handleConfig)

	// 优雅退出：Ctrl+C 时保存数据
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		fmt.Println("\n正在保存数据...")
		app.mu.Lock()
		app.addLog("收到退出信号，正在保存")
		app.saveStats()
		app.saveLogs()
		app.mu.Unlock()
		os.Exit(0)
	}()

	// 启动 HTTP 服务
	port := "8080"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}
	fmt.Printf("DownOnly 已启动 → http://0.0.0.0:%s\n", port)
	http.ListenAndServe("0.0.0.0:"+port, nil)
}