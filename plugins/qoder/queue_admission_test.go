package main

import (
	"net/http"
	"testing"
)

// queueAdmissionBody is a verbatim capture of the gateway's 403 refusal for
// qwen3.8-flash on the qfmodel queue (2026-10-20). The message field holds a
// second JSON document, so every nested quote is backslash-escaped; the
// classifier must still find the business code.
const queueAdmissionBody = `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"modelKey\\\":\\\"qfmodel\\\",\\\"queueCount\\\":0,\\\"queueType\\\":\\\"p3\\\",\\\"retryAfterSeconds\\\":30}\",\"type\":\"server_error\",\"code\":\"internal_server_error\"}}"`

// TestIsQueueAdmissionAcceptsLiveCapture pins the real payload: before this
// change the refusal fell through isSoftRateLimit, the retry loop treated it
// as terminal, and the 403 status made the host report AUTH (a bogus invalid
// API key) instead of a transient queue delay.
func TestIsQueueAdmissionAcceptsLiveCapture(t *testing.T) {
	if !isQueueAdmission(http.StatusForbidden, queueAdmissionBody) {
		t.Fatal("live 10605 capture was not recognised as queue admission")
	}
	if !isSoftRateLimit(http.StatusForbidden, queueAdmissionBody) {
		t.Fatal("queue admission must be a soft rate limit so the retry loop engages")
	}
}

// TestIsQueueAdmissionToleratesEscapingLevels: the same body may arrive
// unescaped (already decoded by an intermediate layer) or single-escaped.
func TestIsQueueAdmissionToleratesEscapingLevels(t *testing.T) {
	cases := map[string]string{
		"plain":          `{"code":"10605","message":"queued"}`,
		"single-escaped": `{\"code\":\"10605\",\"message\":\"queued\"}`,
		"queue-fields":   `{"isQueued":true,"queueType":"p3"}`,
	}
	for name, body := range cases {
		if !isQueueAdmission(http.StatusForbidden, body) {
			t.Errorf("%s: queue admission not recognised", name)
		}
	}
}

// TestIsQueueAdmissionRejectsNonQueue403 keeps genuine credential rejections
// terminal: they must stay AUTH downstream and must not be retried three times
// per request.
func TestIsQueueAdmissionRejectsNonQueue403(t *testing.T) {
	cases := map[string]string{
		"auth":             `{"code":"401","message":"invalid token"}`,
		"forbidden":        `{"message":"access denied"}`,
		"empty":            ``,
		"other-code":       `{"code":"10606","message":"nope"}`,
		"queued-substring": `{"note":"not isQueued here"}`,
	}
	for name, body := range cases {
		if isQueueAdmission(http.StatusForbidden, body) {
			t.Errorf("%s: must not be classified as queue admission", name)
		}
		if isSoftRateLimit(http.StatusForbidden, body) {
			t.Errorf("%s: must not be a soft rate limit", name)
		}
	}
}

// TestIsQueueAdmissionRequires403: the code only means "queued" on a 403. A
// 200 carrying the same text is a payload, not a refusal.
func TestIsQueueAdmissionRequires403(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusTooManyRequests, http.StatusInternalServerError} {
		if isQueueAdmission(status, queueAdmissionBody) {
			t.Errorf("status %d: queue admission must require 403", status)
		}
	}
}

// TestQueueAdmissionOutranksCreditMarkers guards the ordering contract that
// made the first fix ineffective in production. The gateway's 10605 tail is
// truncated in transit, and on a credential with no quota package the body can
// carry credit wording as well. When the credit gate ran first it returned
// true, isSoftRateLimit returned false, and the queue never retried — exactly
// the 403-then-no-retry observed on the Intl credential. Queue admission means
// the request WAS accepted, so it must win over "looks like a credit fault".
func TestQueueAdmissionOutranksCreditMarkers(t *testing.T) {
	body := `{"code":"10605","message":"{\"isQueued\":true,\"queueType\":\"p3\",\"message\":\"额度不足\"}"}`
	if !isHardCreditError(http.StatusForbidden, body) {
		t.Fatal("test body must carry a hard credit marker for this test to mean anything")
	}
	if !isQueueAdmission(http.StatusForbidden, body) {
		t.Fatal("queue admission must still be recognised")
	}
	if !isSoftRateLimit(http.StatusForbidden, body) {
		t.Fatal("a queued request must be retried even when the body also mentions credits")
	}
}

// TestPureCreditErrorStillNotRetried keeps the ordering change from turning
// real credit rejections into retried requests.
func TestPureCreditErrorStillNotRetried(t *testing.T) {
	body := `{"code":"402","message":"积分不足，请充值"}`
	if isSoftRateLimit(http.StatusPaymentRequired, body) {
		t.Fatal("a genuine credit rejection must stay terminal")
	}
}
