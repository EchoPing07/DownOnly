package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// chdirTemp 切换到临时目录并返回还原函数（测试串行执行，无并行冲突）
func chdirTemp(t *testing.T) string {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	return dir
}

// TestConfigUnmarshalLegacyURLs 验证旧版 urls([]string) 自动迁移为 URLEntry，状态标 unknown
func TestConfigUnmarshalLegacyURLs(t *testing.T) {
	raw := `{
		"speed_limit_mbps": 10,
		"daily_quota_min_gb": 100, "daily_quota_max_gb": 200,
		"schedule_start": "00:00", "schedule_end": "23:59",
		"sleep_min_minutes": 10, "sleep_max_minutes": 20,
		"urls": ["http://a.test/x", "http://b.test/y"],
		"allow_public_access": false
	}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if len(c.URLs) != 2 {
		t.Fatalf("expected 2 urls, got %d", len(c.URLs))
	}
	for i, e := range c.URLs {
		if e.Status != statusUnknown {
			t.Errorf("url[%d] status = %q, want %q", i, e.Status, statusUnknown)
		}
		if e.Attempts != 0 {
			t.Errorf("url[%d] attempts = %d, want 0", i, e.Attempts)
		}
	}
	if c.URLs[0].URL != "http://a.test/x" || c.URLs[1].URL != "http://b.test/y" {
		t.Errorf("url order/values wrong: %+v", c.URLs)
	}
}

// TestConfigUnmarshalModernURLs 验证新版 urls([]URLEntry) 保留状态与次数
func TestConfigUnmarshalModernURLs(t *testing.T) {
	raw := `{
		"speed_limit_mbps": 10,
		"urls": [
			{"url":"http://a.test/x","status":"slow","attempts":2},
			{"url":"http://b.test/y","status":"failed","attempts":0}
		]
	}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal modern: %v", err)
	}
	if len(c.URLs) != 2 || c.URLs[0].Status != statusSlow || c.URLs[0].Attempts != 2 {
		t.Errorf("modern urls not preserved: %+v", c.URLs)
	}
}

// TestConfigUnmarshalBlockPublicAccessCompat 验证旧版 block_public_access 反向兼容
func TestConfigUnmarshalBlockPublicAccessCompat(t *testing.T) {
	cases := []struct {
		json     string
		allowPub bool
	}{
		{`{"block_public_access": false}`, true},  // 旧字段 false → 允许公网
		{`{"block_public_access": true}`, false},  // 旧字段 true → 禁止公网
		{`{"allow_public_access": true}`, true},   // 新字段
		{`{"allow_public_access": false}`, false}, // 新字段
		{`{}`, false},                              // 缺省禁止
	}
	for i, tc := range cases {
		var c Config
		if err := json.Unmarshal([]byte(tc.json), &c); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if c.AllowPublicAccess != tc.allowPub {
			t.Errorf("case %d: allow_public_access = %v, want %v", i, c.AllowPublicAccess, tc.allowPub)
		}
	}
}

// TestFixConfig 验证非法值修正与 minSpeed≤limit 约束
func TestFixConfig(t *testing.T) {
	app := &App{}
	app.config = Config{
		SpeedLimitMbps:        10,
		MinSpeedMbps:          50, // 超过 limit，应被夹到 10
		SlowSwitchThreshold:   5,  // 低于 10，应改 60
		SlowSwitchMaxAttempts: 0,  // 应改 3
		URLs: []URLEntry{
			{URL: "http://a", Status: "garbage", Attempts: -5},
			{URL: "http://b", Status: statusNormal},
		},
	}
	app.fixConfig()
	if app.config.MinSpeedMbps != 10 {
		t.Errorf("minSpeed = %d, want 10 (clamped to limit)", app.config.MinSpeedMbps)
	}
	if app.config.SlowSwitchThreshold != 60 {
		t.Errorf("threshold = %d, want 60", app.config.SlowSwitchThreshold)
	}
	if app.config.SlowSwitchMaxAttempts != 3 {
		t.Errorf("maxAttempts = %d, want 3", app.config.SlowSwitchMaxAttempts)
	}
	if app.config.URLs[0].Status != statusUnknown {
		t.Errorf("garbage status should become unknown, got %q", app.config.URLs[0].Status)
	}
	if app.config.URLs[0].Attempts != 0 {
		t.Errorf("negative attempts should become 0, got %d", app.config.URLs[0].Attempts)
	}
	if app.currentSpeedLimit.Load() != 10 {
		t.Errorf("currentSpeedLimit atomic not synced")
	}
}

// TestValidateConfigNewFields 验证新增字段的校验边界
func TestValidateConfigNewFields(t *testing.T) {
	base := Config{
		SpeedLimitMbps:        10,
		MinSpeedMbps:          0,
		SlowSwitchThreshold:   60,
		SlowSwitchMaxAttempts: 3,
		DailyQuotaMinGB:       100, DailyQuotaMaxGB: 200,
		ScheduleStart: "00:00", ScheduleEnd: "23:59",
	}
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"minSpeed negative", func(c *Config) { c.MinSpeedMbps = -1 }, "最低速率"},
		{"minSpeed > limit", func(c *Config) { c.MinSpeedMbps = 11 }, "最低速率"},
		{"threshold too low", func(c *Config) { c.SlowSwitchThreshold = 9 }, "阈值"},
		{"threshold too high", func(c *Config) { c.SlowSwitchThreshold = 91 }, "阈值"},
		{"attempts zero", func(c *Config) { c.SlowSwitchMaxAttempts = 0 }, "尝试次数"},
		{"attempts too high", func(c *Config) { c.SlowSwitchMaxAttempts = 100 }, "尝试次数"},
		{"valid minSpeed=0 disabled", func(c *Config) { c.MinSpeedMbps = 0 }, ""},
		{"valid minSpeed=limit", func(c *Config) { c.MinSpeedMbps = 10 }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.mutate(&c)
			err := validateConfig(c)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("expected error containing %q, got %v", tc.wantErr, err)
				}
			}
		})
	}
}

// TestPickNextURLEOFRound 验证 EOF 后新轮次选源优先级：
// {normal,unknown} > slow > 不选 failed
func TestPickNextURLEOFRound(t *testing.T) {
	app := &App{config: Config{URLs: []URLEntry{
		{URL: "http://failed", Status: statusFailed},
		{URL: "http://slow1", Status: statusSlow},
		{URL: "http://normal1", Status: statusNormal},
		{URL: "http://unknown1", Status: statusUnknown},
		{URL: "http://slow2", Status: statusSlow},
	}}}
	// 多次随机，应始终从 {normal,unknown} 中选
	for i := 0; i < 50; i++ {
		e := app.pickNextURL(false)
		if e == nil {
			t.Fatal("expected non-nil entry")
		}
		if e.Status != statusNormal && e.Status != statusUnknown {
			t.Errorf("got %s, expected normal/unknown when candidates exist", e.Status)
		}
	}

	// 无 normal/unknown，应兑底 slow
	app.config.URLs = []URLEntry{
		{URL: "http://failed", Status: statusFailed},
		{URL: "http://slow1", Status: statusSlow},
	}
	for i := 0; i < 20; i++ {
		e := app.pickNextURL(false)
		if e == nil || e.Status != statusSlow {
			t.Errorf("expected slow fallback, got %+v", e)
		}
	}

	// 全 failed，返回 nil
	app.config.URLs = []URLEntry{{URL: "http://failed", Status: statusFailed}}
	if e := app.pickNextURL(false); e != nil {
		t.Errorf("expected nil when all failed, got %+v", e)
	}
}

// TestPickNextURLSlowSwitch 验证慢切换触发时：候选={normal,unknown}，不兑底 slow
func TestPickNextURLSlowSwitch(t *testing.T) {
	app := &App{config: Config{URLs: []URLEntry{
		{URL: "http://slow1", Status: statusSlow},
		{URL: "http://failed", Status: statusFailed},
	}}}
	// 慢切换：无 normal/unknown 候选 → nil（不兑底 slow）
	if e := app.pickNextURL(true); e != nil {
		t.Errorf("slow switch should not fall back to slow, got %+v", e)
	}

	// 有 normal 候选 → 选 normal
	app.config.URLs = append(app.config.URLs, URLEntry{URL: "http://normal", Status: statusNormal})
	for i := 0; i < 20; i++ {
		e := app.pickNextURL(true)
		if e == nil || e.Status != statusNormal {
			t.Errorf("slow switch should pick normal, got %+v", e)
		}
	}
}

// TestFindURLEntry 验证查找与空结果
func TestFindURLEntry(t *testing.T) {
	app := &App{config: Config{URLs: []URLEntry{
		{URL: "http://a", Status: statusNormal},
		{URL: "http://b", Status: statusSlow, Attempts: 2},
	}}}
	if e := app.findURLEntry("http://b"); e == nil || e.Attempts != 2 {
		t.Errorf("find b failed: %+v", e)
	}
	if e := app.findURLEntry("http://missing"); e != nil {
		t.Errorf("missing should be nil, got %+v", e)
	}
}

// TestApplyTestResult 验证测试结果对状态的影响：
// 通过 → failed/unknown→normal，slow 不变（不可洗白）
// 失败 → normal/unknown→failed，slow/failed 不变
func TestApplyTestResult(t *testing.T) {
	// applyTestResult 内部 saveConfig 写相对路径 data/config.json，
	// 必须隔离到临时目录，否则覆盖仓库真实用户数据
	chdirTemp(t)
	os.MkdirAll(dataDir, 0755)
	cases := []struct {
		name   string
		before string
		ok     bool
		want   string
	}{
		{"failed pass → normal", statusFailed, true, statusNormal},
		{"unknown pass → normal", statusUnknown, true, statusNormal},
		{"normal pass → normal", statusNormal, true, statusNormal},
		{"slow pass → slow (no wash)", statusSlow, true, statusSlow}, // ★ 关键：不可洗白
		{"normal fail → failed", statusNormal, false, statusFailed},
		{"unknown fail → failed", statusUnknown, false, statusFailed},
		{"slow fail → slow (unchanged)", statusSlow, false, statusSlow}, // ★ 不被失败覆盖以外的途径改
		{"failed fail → failed", statusFailed, false, statusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &App{config: Config{URLs: []URLEntry{
				{URL: "http://x", Status: tc.before, Attempts: 1},
			}}}
			app.mu.Lock() // applyTestResult 内部加锁，此处模拟已持锁会死锁——改为直接测试逻辑
			app.mu.Unlock()
			// applyTestResult 自己加锁，直接调用
			app.applyTestResult("http://x", tc.ok)
			got := app.config.URLs[0].Status
			if got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStatusMergeOnConfigPOST 模拟 handleConfig POST 的状态合并逻辑（Bug A 回归测试）：
// 前端发回陈旧快照不应覆盖服务端运行时状态
func TestStatusMergeOnConfigPOST(t *testing.T) {
	// 服务端当前状态：A=slow(att2), B=failed, C=normal
	app := &App{config: Config{URLs: []URLEntry{
		{URL: "http://A", Status: statusSlow, Attempts: 2},
		{URL: "http://B", Status: statusFailed, Attempts: 0},
		{URL: "http://C", Status: statusNormal, Attempts: 0},
	}}}

	// 前端发回的陈旧快照：A=normal(被撤销?), B=normal, C=normal，并新增 D
	incoming := Config{URLs: []URLEntry{
		{URL: "http://A", Status: statusNormal, Attempts: 0}, // 陈旧
		{URL: "http://B", Status: statusNormal, Attempts: 0}, // 陈旧
		{URL: "http://C", Status: statusNormal, Attempts: 0},
		{URL: "http://D", Status: statusNormal, Attempts: 0}, // 新增
	}}

	// 复现 handleConfig POST 的合并逻辑
	app.mu.Lock()
	oldEntries := make(map[string]URLEntry, len(app.config.URLs))
	for _, e := range app.config.URLs {
		oldEntries[e.URL] = e
	}
	for i := range incoming.URLs {
		if old, ok := oldEntries[incoming.URLs[i].URL]; ok {
			incoming.URLs[i].Status = old.Status
			incoming.URLs[i].Attempts = old.Attempts
		} else {
			incoming.URLs[i].Status = statusUnknown
			incoming.URLs[i].Attempts = 0
		}
	}
	app.config = incoming
	app.mu.Unlock()

	want := map[string]struct{ status string; attempts int }{
		"http://A": {statusSlow, 2},   // ★ 保留服务端 slow，不被陈旧 normal 覆盖
		"http://B": {statusFailed, 0}, // ★ 保留服务端 failed
		"http://C": {statusNormal, 0},
		"http://D": {statusUnknown, 0}, // 新增标 unknown
	}
	got := make(map[string]struct{ status string; attempts int })
	for _, e := range app.config.URLs {
		got[e.URL] = struct{ status string; attempts int }{e.Status, e.Attempts}
	}
	for url, w := range want {
		g, ok := got[url]
		if !ok {
			t.Errorf("missing %s", url)
			continue
		}
		if g.status != w.status || g.attempts != w.attempts {
			t.Errorf("%s: got {status=%s att=%d}, want {status=%s att=%d}", url, g.status, g.attempts, w.status, w.attempts)
		}
	}
}

// TestFixConfigSleepZero 回归：validateConfig 允许 sleep 区间为 0（连续下载），
// fixConfig 不得把合法的 0 强制改回默认值（旧代码 <=0 → 10 导致 0 无法生效）
func TestFixConfigSleepZero(t *testing.T) {
	app := &App{}
	app.config = Config{
		SpeedLimitMbps:  5,
		SleepMinMinutes: 0, SleepMaxMinutes: 0,
		ScheduleStart: "00:00", ScheduleEnd: "23:59",
	}
	app.fixConfig()
	if app.config.SleepMinMinutes != 0 || app.config.SleepMaxMinutes != 0 {
		t.Errorf("sleep 0/0 should be preserved, got %d/%d",
			app.config.SleepMinMinutes, app.config.SleepMaxMinutes)
	}

	app.config.SleepMinMinutes = -5
	app.config.SleepMaxMinutes = -1
	app.fixConfig()
	if app.config.SleepMinMinutes != 10 || app.config.SleepMaxMinutes != 20 {
		t.Errorf("negative sleep should be corrected to 10/20, got %d/%d",
			app.config.SleepMinMinutes, app.config.SleepMaxMinutes)
	}

	app.config.SleepMinMinutes = 30
	app.config.SleepMaxMinutes = 10
	app.fixConfig()
	if app.config.SleepMaxMinutes != 30 {
		t.Errorf("sleep max < min should clamp to min, got %d", app.config.SleepMaxMinutes)
	}
}

// TestValidateConfigSleepZero 验证 0/0 通过整体配置校验
func TestValidateConfigSleepZero(t *testing.T) {
	cfg := Config{
		SpeedLimitMbps: 5, SlowSwitchThreshold: 60, SlowSwitchMaxAttempts: 3,
		DailyQuotaMinGB: 100, DailyQuotaMaxGB: 200,
		ScheduleStart: "00:00", ScheduleEnd: "23:59",
		SleepMinMinutes: 0, SleepMaxMinutes: 0,
	}
	if err := validateConfig(cfg); err != nil {
		t.Errorf("sleep 0/0 should validate, got %v", err)
	}
}

// TestIsAllowedURLEdges 覆盖 SSRF 校验边界：未指定地址/环回/链路本地/协议白名单/公网 IP
func TestIsAllowedURLEdges(t *testing.T) {
	denied := []string{
		"http://0.0.0.0/x",           // Linux 上等价连接本机，曾漏判
		"http://127.0.0.1/x",
		"http://localhost/x",
		"http://[::1]/x",
		"http://169.254.169.254/x",   // 云元数据
		"http://192.168.1.1/x",
		"http://10.0.0.1/x",
		"http://172.16.0.1/x",
		"http://[fe80::1]/x",
		"ftp://8.8.8.8/x",            // 非 HTTP(S)
		"file:///etc/passwd",
		"/relative/path",
	}
	for _, u := range denied {
		if ok, _ := isAllowedURL(u); ok {
			t.Errorf("isAllowedURL(%q) = true, want false", u)
		}
	}
	if ok, transient := isAllowedURL("http://8.8.8.8/x"); !ok || transient {
		t.Errorf("isAllowedURL(public IP) = (%v, %v), want (true, false)", ok, transient)
	}
}

// TestLoadConfigFirstRun 回归：首次运行（无 config.json）也必须走 fixConfig，
// 否则 currentSpeedLimit 保持 0，doDownload 中 bytesPerSec=0 会导致节流计算异常
func TestLoadConfigFirstRun(t *testing.T) {
	chdirTemp(t)
	os.MkdirAll(dataDir, 0755) // main() 在 loadConfig 前创建数据目录
	app := &App{}
	app.loadConfig()
	if got := app.currentSpeedLimit.Load(); got != int64(app.config.SpeedLimitMbps) {
		t.Errorf("currentSpeedLimit = %d, want %d", got, app.config.SpeedLimitMbps)
	}
	if len(app.config.URLs) == 0 {
		t.Error("first run should get default URL list")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "config.json")); err != nil {
		t.Errorf("config.json should be created on first run: %v", err)
	}
}

// TestLoadConfigCorruptBackup 回归：config.json 损坏时应备份原文件再回退默认值，
// 不允许直接用空配置覆盖写盘导致用户源列表永久丢失
func TestLoadConfigCorruptBackup(t *testing.T) {
	dir := chdirTemp(t)
	os.MkdirAll(dataDir, 0755)
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"urls": [broken`), 0600); err != nil {
		t.Fatal(err)
	}
	app := &App{config: Config{URLs: []URLEntry{{URL: "http://user-saved"}}}}
	app.loadConfig()

	// 回退到默认配置而非零值
	if len(app.config.URLs) == 0 || app.config.URLs[0].URL == "http://user-saved" {
		t.Errorf("corrupt config should fall back to defaults, got %+v", app.config.URLs)
	}
	// 损坏文件被备份而非覆盖
	backups, _ := filepath.Glob(filepath.Join(dir, dataDir, "config.json.corrupt-*"))
	if len(backups) != 1 {
		t.Fatalf("expected 1 corrupt backup, got %d", len(backups))
	}
	// 重新生成的 config.json 是合法 JSON
	data, err := os.ReadFile(filepath.Join(dataDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		t.Errorf("rewritten config.json is not valid JSON: %v", err)
	}
}

// TestLoadStatsCorruptBackup 验证 stats.json 损坏同样备份后重置
func TestLoadStatsCorruptBackup(t *testing.T) {
	dir := chdirTemp(t)
	os.MkdirAll(dataDir, 0755)
	os.WriteFile(filepath.Join(dataDir, "stats.json"), []byte(`{bad json`), 0600)
	app := &App{}
	app.loadStats()
	if app.stats.Daily == nil || app.stats.TodayDate == "" {
		t.Errorf("stats should reset to defaults, got %+v", app.stats)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, dataDir, "stats.json.corrupt-*"))
	if len(backups) != 1 {
		t.Fatalf("expected 1 corrupt backup, got %d", len(backups))
	}
}

// TestLoadConfigZeroedSelfHeals 回归：config.json 被测试污染成全零/空时间后，
// loadConfig+fixConfig 必须把配置修复到能通过 validateConfig 的状态，
// 否则 GET /api/config 返回的配置无法通过 POST 自身校验，
// 前端"添加地址"整体回传配置时被 400 拒绝且静默失败
func TestLoadConfigZeroedSelfHeals(t *testing.T) {
	chdirTemp(t)
	os.MkdirAll(dataDir, 0755)
	zeroed := `{
		"speed_limit_mbps": 0, "min_speed_mbps": 0,
		"slow_switch_threshold": 0, "slow_switch_max_attempts": 0,
		"daily_quota_min_gb": 0, "daily_quota_max_gb": 0,
		"schedule_start": "", "schedule_end": "",
		"sleep_min_minutes": 0, "sleep_max_minutes": 0,
		"urls": [{"url": "http://x", "status": "failed", "attempts": 1}]
	}`
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(zeroed), 0600); err != nil {
		t.Fatal(err)
	}
	app := &App{}
	app.loadConfig()
	if err := validateConfig(app.config); err != nil {
		t.Fatalf("被污染配置加载后应自愈并通过校验: %v", err)
	}
	if app.config.ScheduleStart != "00:00" || app.config.ScheduleEnd != "23:59" {
		t.Errorf("schedule = %q-%q, want 00:00-23:59",
			app.config.ScheduleStart, app.config.ScheduleEnd)
	}
}

// TestWriteFileAtomic 验证原子写内容完整且不残留临时文件
func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	if err := writeFileAtomic(path, []byte(`{"k":1}`), 0600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"k":1}` {
		t.Errorf("content mismatch: %q, %v", data, err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".test.json.tmp-*"))
	if len(leftovers) != 0 {
		t.Errorf("temp files leaked: %v", leftovers)
	}
}

// --- issue #13：持久化写放大与内存优化回归测试 ---

// TestSaveConfigDedup 回归：内容未变时 saveConfig 不得写盘
// （修复前下载热循环每秒重写 config.json 约 8 次，绝大多数内容与上次完全相同）
func TestSaveConfigDedup(t *testing.T) {
	chdirTemp(t)
	os.MkdirAll(dataDir, 0755)
	app := &App{}
	app.config = defaultConfig()
	app.fixConfig()
	app.saveConfig()
	if _, err := os.Stat(filepath.Join(dataDir, "config.json")); err != nil {
		t.Fatalf("首次 saveConfig 应创建文件: %v", err)
	}
	// 内容未变：删除文件后再 saveConfig，不应重建
	os.Remove(filepath.Join(dataDir, "config.json"))
	app.saveConfig()
	if _, err := os.Stat(filepath.Join(dataDir, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("内容未变时不应写盘, stat err=%v", err)
	}
	// 内容变化：恢复写盘
	app.config.SpeedLimitMbps = 42
	app.saveConfig()
	data, err := os.ReadFile(filepath.Join(dataDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if json.Unmarshal(data, &c) != nil || c.SpeedLimitMbps != 42 {
		t.Errorf("内容变化后应写入新值, got %s", data)
	}
}

// TestSaveStatsDedup 回归：内容未变时 saveStats 不得写盘（修复前空闲期每 60s 空写一次）
func TestSaveStatsDedup(t *testing.T) {
	chdirTemp(t)
	os.MkdirAll(dataDir, 0755)
	app := &App{}
	app.stats = Stats{Daily: map[string]uint64{}, TodayDate: "2026-10-24"}
	app.saveStats()
	os.Remove(filepath.Join(dataDir, "stats.json"))
	app.saveStats()
	if _, err := os.Stat(filepath.Join(dataDir, "stats.json")); !os.IsNotExist(err) {
		t.Fatal("内容未变时不应写盘")
	}
	app.stats.TodayBytes = 123
	app.saveStats()
	if _, err := os.Stat(filepath.Join(dataDir, "stats.json")); err != nil {
		t.Fatal("内容变化后应写盘")
	}
}

// TestAddLogAppendsJSONL 回归：日志逐条追加（不再整文件重写），文件与内存一致
func TestAddLogAppendsJSONL(t *testing.T) {
	chdirTemp(t)
	os.MkdirAll(logsDir, 0755)
	app := &App{todayLogsDate: "2026-10-24"}
	for i := 0; i < 5; i++ {
		app.addLog(fmt.Sprintf("条目 %d", i))
	}
	app.closeLogFile()
	data, err := os.ReadFile(logFilePath("2026-10-24"))
	if err != nil {
		t.Fatalf("jsonl 未创建: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 5 {
		t.Fatalf("want 5 lines, got %d", len(lines))
	}
	for _, line := range lines {
		var e LogEntry
		if json.Unmarshal([]byte(line), &e) != nil || e.Time == "" || e.Msg == "" {
			t.Errorf("行不是合法 LogEntry: %q", line)
		}
	}
	loaded := loadLogsFromFile("2026-10-24")
	if len(loaded) != 5 || loaded[0].Msg != "条目 0" || loaded[4].Msg != "条目 4" {
		t.Errorf("加载结果与写入不一致: %+v", loaded)
	}
}

// TestLoadLogsLegacyCompat 验证旧版整文件 JSON 格式仍可读取
func TestLoadLogsLegacyCompat(t *testing.T) {
	chdirTemp(t)
	os.MkdirAll(logsDir, 0755)
	os.WriteFile(legacyLogFilePath("2026-10-23"),
		[]byte(`{"entries":[{"time":"01:02:03","msg":"旧格式"}]}`), 0600)
	entries := loadLogsFromFile("2026-10-23")
	if len(entries) != 1 || entries[0].Msg != "旧格式" {
		t.Fatalf("旧格式读取失败: %+v", entries)
	}
}

// TestMigrateLegacyTodayLog 验证启动迁移：旧格式当日日志转 JSONL 后删除旧文件，且幂等
func TestMigrateLegacyTodayLog(t *testing.T) {
	chdirTemp(t)
	os.MkdirAll(logsDir, 0755)
	os.WriteFile(legacyLogFilePath("2026-10-24"),
		[]byte(`{"entries":[{"time":"01:02:03","msg":"a"},{"time":"01:02:04","msg":"b"}]}`), 0600)
	app := &App{todayLogsDate: "2026-10-24"}
	app.migrateLegacyTodayLog()
	if _, err := os.Stat(legacyLogFilePath("2026-10-24")); !os.IsNotExist(err) {
		t.Error("旧文件应被删除")
	}
	if got := len(loadLogsFromFile("2026-10-24")); got != 2 {
		t.Fatalf("迁移后应有 2 条, got %d", got)
	}
	// 旧文件已删，重复迁移应无操作
	app.migrateLegacyTodayLog()
	if got := len(loadLogsFromFile("2026-10-24")); got != 2 {
		t.Fatalf("重复迁移不应重复导入, got %d", got)
	}
}

// TestGetLogDatesDualSuffix 验证日期列表同时识别新版 .jsonl 与旧版 .json
func TestGetLogDatesDualSuffix(t *testing.T) {
	chdirTemp(t)
	os.MkdirAll(logsDir, 0755)
	os.WriteFile(logFilePath("2026-10-24"), []byte("\n"), 0600)
	os.WriteFile(legacyLogFilePath("2026-10-23"), []byte("{}"), 0600)
	app := &App{todayLogsDate: "2026-10-24"}
	dates := app.getLogDates()
	if len(dates) != 2 || dates[0] != "2026-10-24" || dates[1] != "2026-10-23" {
		t.Errorf("日期列表错误: %v", dates)
	}
}

// TestProgressAggregation 验证高频小文件的"下载完成"聚合输出与事件日志的时序
func TestProgressAggregation(t *testing.T) {
	chdirTemp(t)
	os.MkdirAll(logsDir, 0755)
	app := &App{todayLogsDate: "2026-10-24"}
	for i := 0; i < 27; i++ {
		app.addProgress("http://a", 2000)
	}
	if len(app.todayLogs) != 0 {
		t.Fatalf("聚合窗口内不应产生日志, got %d 条", len(app.todayLogs))
	}
	app.flushProgress()
	if len(app.todayLogs) != 1 {
		t.Fatalf("聚合后应输出 1 条, got %d", len(app.todayLogs))
	}
	want := fmt.Sprintf("下载完成: %s × %d 次", formatBytes(27*2000), 27)
	if app.todayLogs[0].Msg != want {
		t.Errorf("got %q, want %q", app.todayLogs[0].Msg, want)
	}
	// 事件日志应先冲掉聚合中的进度，保持时序
	app.addProgress("http://a", 100)
	app.addLog("服务已停止")
	if len(app.todayLogs) != 3 {
		t.Fatalf("应有 3 条（聚合1+完成1+事件1), got %d", len(app.todayLogs))
	}
	if app.todayLogs[1].Msg != fmt.Sprintf("下载完成: %s", formatBytes(100)) {
		t.Errorf("单条完成文案错误: %q", app.todayLogs[1].Msg)
	}
	if app.todayLogs[2].Msg != "服务已停止" {
		t.Errorf("事件应在聚合之后: %q", app.todayLogs[2].Msg)
	}
	app.closeLogFile() // Windows 下必须关闭句柄，TempDir 才能清理
}

// TestLowSpeedRatioIgnoresIdle 回归：空闲哨兵不计入慢判定占比
// （修复前休眠/空闲秒被计为低速，导致好源被误标 slow）
func TestLowSpeedRatioIgnoresIdle(t *testing.T) {
	hist := make([]float64, 60)
	for i := 0; i < 30; i++ { hist[i] = -1 } // 30 秒空闲
	for i := 30; i < 60; i++ { hist[i] = 0.5 } // 30 秒低速
	if got := lowSpeedRatio(hist, 1); got != 100 {
		t.Errorf("idle+low → got %d%%, want 100%%（空闲不应稀释占比）", got)
	}
	for i := range hist { hist[i] = -1 } // 全空闲
	if got := lowSpeedRatio(hist, 1); got != 0 {
		t.Errorf("all idle → got %d%%, want 0%%（不触发切换）", got)
	}
	for i := 0; i < 30; i++ { hist[i] = 5 } // 30 高速
	for i := 30; i < 60; i++ { hist[i] = 0.5 } // 30 低速（无空闲）
	if got := lowSpeedRatio(hist, 1); got != 50 {
		t.Errorf("mixed → got %d%%, want 50%%", got)
	}
}

// TestSpeedHistorySanitized 验证 handleStatus 输出的历史不含空闲哨兵负值
func TestSpeedHistorySanitized(t *testing.T) {
	app := &App{speedHistory: []float64{5, -1, -1, 3, 0}}
	w := httptest.NewRecorder()
	app.handleStatus(w, httptest.NewRequest("GET", "/api/status", nil))
	var resp struct {
		SpeedHistory []float64 `json:"speed_history"`
		SpeedMbps    float64   `json:"speed_mbps"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for i, v := range resp.SpeedHistory {
		if v < 0 {
			t.Fatalf("speed_history[%d]=%v 含负值: %v", i, v, resp.SpeedHistory)
		}
	}
	if resp.SpeedHistory[1] != 0 || resp.SpeedHistory[2] != 0 {
		t.Errorf("哨兵应输出为 0: %v", resp.SpeedHistory)
	}
}

// TestTransportReuse 回归：复用包级 Transport 后连续下载不泄漏 goroutine
// （修复前每次 doDownload 新建 Transport，100 秒内泄漏 239 对 readLoop/writeLoop）
func TestTransportReuse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello-downonly"))
	}))
	defer srv.Close()

	app := &App{}
	app.isRunning.Store(true)
	app.currentSpeedLimit.Store(int64(100))

	if _, err := app.doDownload(srv.URL); err != nil {
		t.Fatalf("first download: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // 等连接的 readLoop/writeLoop 稳定，计入基线
	base := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		if _, err := app.doDownload(srv.URL); err != nil {
			t.Fatalf("download %d: %v", i, err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if grew := runtime.NumGoroutine() - base; grew > 4 {
		t.Errorf("goroutine 增长 %d（复用 Transport 应≈0），存在连接泄漏", grew)
	}
}

// errWriter 模拟磁盘满等底层写失败
type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, errors.New("disk full") }

// TestAddLogWriteErrorDropsHandle 回归：日志写入失败后必须丢弃句柄下次重开，
// 否则 bufio 粘性错误会让当天文件日志静默失效（只有日期切换才恢复）
func TestAddLogWriteErrorDropsHandle(t *testing.T) {
	chdirTemp(t)
	os.MkdirAll(logsDir, 0755)
	app := &App{todayLogsDate: "2026-10-24"}
	app.addLog("正常条目A")
	app.closeLogFile()

	// 注入必失败的底层写入器；超缓冲大条目迫使 Encode 立即触发底层写并报错
	app.logBuf = bufio.NewWriter(errWriter{})
	app.logEnc = json.NewEncoder(app.logBuf)
	app.addLog(strings.Repeat("x", 8192))
	if app.logEnc != nil {
		t.Fatal("写入失败后应丢弃句柄")
	}

	app.addLog("恢复条目B")
	app.closeLogFile()
	data, err := os.ReadFile(logFilePath("2026-10-24"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "正常条目A") || !strings.Contains(string(data), "恢复条目B") {
		t.Errorf("恢复后应继续追加真实文件: %s", data)
	}
}

// TestCheckDateChangeRollover 回归：日期切换时昨日文件封存、
// 新条目写入新日期文件、内存与流量归档同步切换
func TestCheckDateChangeRollover(t *testing.T) {
	chdirTemp(t)
	os.MkdirAll(dataDir, 0755)
	os.MkdirAll(logsDir, 0755)
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	today := time.Now().Format("2006-01-02")
	app := &App{
		todayLogsDate: yesterday,
		stats:         Stats{Daily: map[string]uint64{}, TodayDate: yesterday, TodayBytes: 123},
	}
	app.config = defaultConfig()
	app.addLog("昨日条目")
	app.checkDateChange()
	app.flushLogs() // 当日新文件仍在缓冲中

	yData, err := os.ReadFile(logFilePath(yesterday))
	if err != nil || !strings.Contains(string(yData), "昨日条目") {
		t.Errorf("昨日文件应保留条目: %q, %v", yData, err)
	}
	tData, err := os.ReadFile(logFilePath(today))
	if err != nil || !strings.Contains(string(tData), "日期更新") {
		t.Errorf("新日期文件应收到日期更新日志: %q, %v", tData, err)
	}
	if app.todayLogsDate != today || len(app.todayLogs) == 0 {
		t.Errorf("内存应切换到新日期: date=%q logs=%d", app.todayLogsDate, len(app.todayLogs))
	}
	if app.stats.Daily[yesterday] != 123 || app.stats.TodayBytes != 0 {
		t.Errorf("昨日流量应归档: daily=%v today=%d", app.stats.Daily, app.stats.TodayBytes)
	}
	app.closeLogFile()
}

// TestLoadLogsTailCap 验证加载超过上限的历史日志时只保留最近 logCap 条
func TestLoadLogsTailCap(t *testing.T) {
	chdirTemp(t)
	os.MkdirAll(logsDir, 0755)
	var b strings.Builder
	for i := 0; i < logCap+5; i++ {
		fmt.Fprintf(&b, "{\"time\":\"00:00:00\",\"msg\":\"%d\"}\n", i)
	}
	if err := os.WriteFile(logFilePath("2026-10-24"), []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
	entries := loadLogsFromFile("2026-10-24")
	if len(entries) != logCap {
		t.Fatalf("want %d entries, got %d", logCap, len(entries))
	}
	if entries[0].Msg != "5" {
		t.Errorf("应保留最后 %d 条, first=%q", logCap, entries[0].Msg)
	}
}

// TestLogDownloadStartSuppress 验证同源连续轮次不重复记录"开始下载"，换源时先冲聚合
func TestLogDownloadStartSuppress(t *testing.T) {
	chdirTemp(t)
	os.MkdirAll(logsDir, 0755)
	app := &App{todayLogsDate: "2026-10-24"}
	app.logDownloadStart("http://a")
	app.addProgress("http://a", 10)
	app.logDownloadStart("http://a") // 同源连续轮次：不重复记录
	if len(app.todayLogs) != 1 {
		t.Fatalf("同源重复开始不应记录, got %d 条", len(app.todayLogs))
	}
	app.logDownloadStart("http://b") // 换源：先输出聚合再记录
	if len(app.todayLogs) != 3 {
		t.Fatalf("换源应输出聚合+开始共 3 条, got %d", len(app.todayLogs))
	}
	if app.todayLogs[1].Msg != fmt.Sprintf("下载完成: %s", formatBytes(10)) {
		t.Errorf("聚合应在开始日志之前: %q", app.todayLogs[1].Msg)
	}
	app.closeLogFile()
}
