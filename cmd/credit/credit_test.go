package main

import "testing"

// TestPackageRemainUsedCycle 计费包的 Cycle* 分支：有周期字段时以周期为准。
//
// 这是面板/日报上「剩余多少积分」的唯一口径来源，与 internal/upstream/billing.go
// 的聚合逻辑必须一致——两边算错一边，用户就会看到两套数字。
func TestPackageRemainUsedCycle(t *testing.T) {
	cases := []struct {
		name         string
		in           resourcePackage
		remain, used int64
		size         int64
	}{
		{
			name:   "周期字段齐全时取周期值，不看 Capacity*",
			in:     resourcePackage{CycleCapacitySize: 1000, CycleCapacityRemain: 300, CapacitySize: 9999, CapacityRemain: 8888},
			remain: 300, used: 700, size: 1000,
		},
		{
			// 注意：这里最终值由下面的 CycleUsed 反推保底（used=size-remain 变负 → 任何
			// 非负 CycleUsed 都会触发重算），`remain > size` 那行钳制单独摘掉测不出来。
			name:   "剩余超过总量时结果仍在总量内",
			in:     resourcePackage{CycleCapacitySize: 1000, CycleCapacityRemain: 1500},
			remain: 1000, used: 0, size: 1000,
		},
		{
			name:   "剩余为负要归零，且已用按总量计",
			in:     resourcePackage{CycleCapacitySize: 1000, CycleCapacityRemain: -20},
			remain: 0, used: 1000, size: 1000,
		},
		{
			name:   "周期已用更大时以已用为准反推剩余",
			in:     resourcePackage{CycleCapacitySize: 1000, CycleCapacityRemain: 800, CycleCapacityUsed: 400},
			remain: 600, used: 400, size: 1000,
		},
		{
			name:   "周期已用超过总量（超支）时不再反推剩余",
			in:     resourcePackage{CycleCapacitySize: 1000, CycleCapacityRemain: 0, CycleCapacityUsed: 1200},
			remain: 0, used: 1200, size: 1000,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			remain, used, size := packageRemainUsed(c.in)
			if remain != c.remain || used != c.used || size != c.size {
				t.Fatalf("packageRemainUsed = (%d,%d,%d)，want (%d,%d,%d)",
					remain, used, size, c.remain, c.used, c.size)
			}
			// 不变量：剩余必须落在 [0, size]，否则日报的百分比会出现负数或 >100%。
			if remain < 0 || remain > size {
				t.Fatalf("剩余 %d 越界，应在 [0,%d]", remain, size)
			}
		})
	}
}

// TestPackageRemainUsedLegacy 无周期字段时回落到 Capacity*（老套餐）。
func TestPackageRemainUsedLegacy(t *testing.T) {
	cases := []struct {
		name         string
		in           resourcePackage
		remain, used int64
		size         int64
	}{
		{
			name:   "未给已用时由总量减剩余补出",
			in:     resourcePackage{CapacitySize: 200, CapacityRemain: 50},
			remain: 50, used: 150, size: 200,
		},
		{
			name:   "已给已用时原样保留（不再覆盖）",
			in:     resourcePackage{CapacitySize: 200, CapacityRemain: 50, CapacityUsed: 10},
			remain: 50, used: 10, size: 200,
		},
		{
			name:   "全零包不产生数字",
			in:     resourcePackage{},
			remain: 0, used: 0, size: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			remain, used, size := packageRemainUsed(c.in)
			if remain != c.remain || used != c.used || size != c.size {
				t.Fatalf("packageRemainUsed = (%d,%d,%d)，want (%d,%d,%d)",
					remain, used, size, c.remain, c.used, c.size)
			}
		})
	}
}

// TestBillingBaseFor global 账号必须打国际版计费域。
//
// 这里曾经把域硬编码成 CN，结果 global 账号余额恒返回 401（面板显示的是池内缓存值）。
// 判定口径要与 internal/auth.Realm() 一致。
func TestBillingBaseFor(t *testing.T) {
	cases := []struct {
		name  string
		realm string
		dom   string
		want  string
	}{
		{name: "显式 realm=global", realm: "global", want: billingBaseGlobal},
		{name: "realm 大小写与空格不敏感", realm: "  GLOBAL ", want: billingBaseGlobal},
		{name: "domain 是 workbuddy.ai", dom: "workbuddy.ai", want: billingBaseGlobal},
		{name: "domain 是子域", dom: "api.workbuddy.ai", want: billingBaseGlobal},
		{name: "domain 大写子域", dom: "API.WorkBuddy.AI", want: billingBaseGlobal},
		{name: "CN 域回落国内", dom: "codebuddy.cn", want: billingBaseCN},
		{name: "全空回落国内", want: billingBaseCN},
		{name: "realm 是 cn 时看 domain", realm: "cn", dom: "workbuddy.ai", want: billingBaseGlobal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			af := &authFile{}
			af.Auth.Realm = c.realm
			af.Auth.Domain = c.dom
			if got := billingBaseFor(af); got != c.want {
				t.Fatalf("billingBaseFor(realm=%q,domain=%q) = %q，want %q", c.realm, c.dom, got, c.want)
			}
		})
	}
}
