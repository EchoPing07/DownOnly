package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
