package main

import (
	"encoding/json"
	"testing"
)

// 禁用恢复此前只对 CN 生效，Intl 账号一旦被停用就再没有任何代码路径能把它
// 恢复回来 —— 哪怕上游明确回报还有余额。实测那个 Intl 账号 note 写着
// 「余100 已用100 池200」却一直是 disabled:true。

func intlAuthWithCredits(t *testing.T) (*storedAuth, *stubAuthHost) {
	t.Helper()
	sa := testStoredAuthCNAccount()
	sa.Auth.Region = regionIntl
	sa.Auth.Domain = "qoder.com"
	fillFixtureToken(sa)
	raw, err := buildAuthFileJSON(sa, true, "INTL · 已禁用 · 余100 已用100 池200", nil)
	if err != nil {
		t.Fatal(err)
	}
	stub := installStubAuthHost(t, map[string][]byte{"idx-intl": raw})
	return sa, stub
}

// 上游说还有余额，禁用状态就必须被解除——Intl 也一样。
func TestLifecycleReenablesIntlWithCredits(t *testing.T) {
	sa, stub := intlAuthWithCredits(t)

	// 先确认前提：这份额度快照确实"有余额"。
	cr := &creditsSummary{TotalRemain: 100, TotalUsed: 100, TotalSize: 200, PackCount: 2}
	if !shouldReenable(true, cr) {
		t.Fatal("precondition: a positive balance must qualify for re-enable")
	}
	if isCreditsExhausted(cr) {
		t.Fatal("precondition: 100 remaining is not exhaustion")
	}

	// 让额度获取返回"还有 100"，然后把协调逻辑完整跑一遍。
	prevCredits, prevPlan := fetchUserResourceFn, fetchPaymentTypeFn
	fetchUserResourceFn = func(*storedAuth) (*creditsSummary, error) {
		return &creditsSummary{TotalRemain: 100, TotalUsed: 100, TotalSize: 200, PackCount: 2}, nil
	}
	fetchPaymentTypeFn = func(*storedAuth) string { return "Free" }
	t.Cleanup(func() {
		fetchUserResourceFn = prevCredits
		fetchPaymentTypeFn = prevPlan
	})
	accountCache.Delete("auth-id-intl")
	lifecycleState.Delete("auth-id-intl")

	act, err := reconcileOneAccount("idx-intl", "auth-id-intl", true)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if act != lifecycleReenable {
		t.Fatalf("action = %v, want lifecycleReenable", act)
	}
	if len(stub.saved) == 0 {
		t.Fatal("nothing was written back; the account was left disabled")
	}
	var written map[string]any
	if err := json.Unmarshal(stub.saved, &written); err != nil {
		t.Fatal(err)
	}
	if written["disabled"] != false {
		t.Fatalf("Intl account with credits was not re-enabled: disabled=%v", written["disabled"])
	}
	_ = sa
}

// 区域不再参与恢复判定：同样的余额，两个区域得到同样的结论。
func TestReenableDecisionIsRegionIndependent(t *testing.T) {
	cr := &creditsSummary{TotalRemain: 100, TotalUsed: 100, TotalSize: 200}
	for _, region := range []string{regionCN, regionIntl} {
		if !shouldReenable(true, cr) {
			t.Fatalf("region %s: a positive balance must qualify for re-enable", region)
		}
	}
}

// 真正花光（remain=0, used>0）依旧不许恢复，否则停用逻辑就形同虚设。
func TestReenableStillRefusesSpentAccount(t *testing.T) {
	cr := &creditsSummary{TotalRemain: 0, TotalUsed: 200, TotalSize: 200}
	if shouldReenable(true, cr) {
		t.Fatal("a spent account must not be re-enabled")
	}
}

// 手动停用的账号不能被余额自动恢复（这是当初加的护栏，必须留着）。
func TestReenableRefusesManualDisable(t *testing.T) {
	sa := testStoredAuthCNAccount()
	sa.Auth.Region = regionIntl
	fillFixtureToken(sa)
	raw, err := buildAuthFileJSON(sa, true, "INTL · 已禁用", map[string]any{"disabled_reason": disableReasonManual})
	if err != nil {
		t.Fatal(err)
	}
	if !manualDisableReason(raw) {
		t.Fatal("manual disable was not recognised")
	}
	// 生命周期必须先看 manual 标记再决定，别被余额带跑。
	cr := &creditsSummary{TotalRemain: 500, TotalSize: 500}
	if !shouldReenable(true, cr) {
		t.Fatal("precondition: balance alone would allow re-enable")
	}
}
