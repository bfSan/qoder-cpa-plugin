package main

import "testing"

// qoder's remain/used/size all derive from the same upstream objects
// (q.UserQuota and q.AddOnQuota, summed), so they cannot disagree with each
// other today. This test is not about catching a present bug — it is about
// making sure the panel stops silently absorbing one.
//
// The line this replaces was `if size > total { total = size }`, copied from the
// workbuddy plugin, where it concealed a real accounting bug: TotalDosage, a
// lifetime counter, was being used as the size floor and then subtracted from a
// remain that counted only spendable packages. The result was a panel reading
// 余10199 已用99500 池109699 for an account whose packages summed to 14729, and
// nothing on screen said so.
func TestSummarizeCreditsFlagsInconsistentTotals(t *testing.T) {
	ok := summarizeCredits([]wbAccount{{
		Credits: &creditsSummary{TotalRemain: 1500, TotalUsed: 0, TotalSize: 1500},
	}})
	if got := ok["inconsistent"].(int64); got != 0 {
		t.Errorf("inconsistent = %d, want 0 for consistent figures", got)
	}
	if got := ok["total"].(int64); got != 1500 {
		t.Errorf("total = %d, want 1500", got)
	}

	// The shape the workbuddy bug produced, asserted here so that if qoder's
	// upstream ever diverges the panel says so instead of rendering a plausible
	// bar over nonsense.
	bad := summarizeCredits([]wbAccount{{
		Credits: &creditsSummary{TotalRemain: 10194, TotalUsed: 4535, TotalSize: 109699},
	}})
	if got := bad["inconsistent"].(int64); got != 1 {
		t.Errorf("inconsistent = %d, want 1; the disagreement must be visible", got)
	}
	if got := bad["total"].(int64); got != 109699 {
		t.Errorf("total = %d, want the larger figure 109699", got)
	}
}

// A quota of zero is a real state, not a disagreement, so it must not warn.
func TestSummarizeCreditsZeroSizeIsNotInconsistent(t *testing.T) {
	sum := summarizeCredits([]wbAccount{{
		Credits: &creditsSummary{TotalRemain: 40, TotalUsed: 10},
	}})
	if got := sum["inconsistent"].(int64); got != 0 {
		t.Errorf("inconsistent = %d, want 0 when size is 0", got)
	}
	if got := sum["total"].(int64); got != 50 {
		t.Errorf("total = %d, want 50", got)
	}
}

// Accounts with no credit data at all are skipped, and a fully consistent set
// must not warn.
func TestSummarizeCreditsIgnoresUnknownAccounts(t *testing.T) {
	sum := summarizeCredits([]wbAccount{
		{Region: "cn", Disabled: true},
		{Region: "global", Credits: &creditsSummary{TotalRemain: 200, TotalUsed: 0, TotalSize: 200}},
	})
	if got := sum["known_count"].(int); got != 1 {
		t.Errorf("known_count = %d, want 1", got)
	}
	if got := sum["inconsistent"].(int64); got != 0 {
		t.Errorf("inconsistent = %d, want 0", got)
	}
	if got := sum["global_remain"].(int64); got != 200 {
		t.Errorf("global_remain = %d, want 200", got)
	}
}
