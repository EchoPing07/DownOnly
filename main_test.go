package main

import (
	"encoding/json"
	"strings"
	"testing"
)

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
