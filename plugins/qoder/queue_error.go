package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// queueAdmissionError is the public representation of Qoder's business-level
// queue refusal. Never include the raw body: it contains an HTTP auth status
// even though the credential was accepted, and clients classify that text as
// an invalid API key. Internal retry status is retained separately; public
// errors intentionally retain only the safe retry hint, not the raw body.
type queueAdmissionError struct {
	retryAfterSeconds int
	hint              quotaHint
}

func (e *queueAdmissionError) Error() string {
	return fmt.Sprintf("qoder rate limit: 模型排队中，上游资源暂不可用，请在 %d 秒后重试 (RESOURCE_LIMIT; retryAfterSeconds=%d; unexpected EOF)", e.retryAfterSeconds, e.retryAfterSeconds)
}

// The host ABI cannot separate a public HTTP status from credential cooldown.
// Do not add StatusCode or unwrap the original refusal here: either would make
// a transient model queue cool the entire credential. The lifecycle marker is
// required by the deployed host; the plugin still cools the affected model.
func queueErrorForClient(err error) error {
	var daily *dailyQuotaError
	if errors.As(err, &daily) {
		return daily
	}
	var queued *queueAdmissionError
	if errors.As(err, &queued) {
		return queued
	}
	return quotaErrorForClient(err)
}

var queueCodePattern = regexp.MustCompile(`"code"\s*:\s*(?:"10605"|10605)\s*[,}]`)

var queueRetryAfterPattern = regexp.MustCompile(`"retryAfterSeconds"\s*:\s*"?([0-9]+)`)

// newQueueAdmissionError must inspect the full body before log redaction or
// truncation. Qoder nests JSON strings, sometimes truncating the final envelope;
// flattening escapes lets us retain the numeric retry hint in either case.
func newQueueAdmissionError(status int, body string) *queueAdmissionError {
	if !isQueueAdmission(status, body) {
		return nil
	}
	seconds := 30
	flat := strings.ReplaceAll(body, `\`, "")
	if match := queueRetryAfterPattern.FindStringSubmatch(flat); len(match) == 2 {
		if n, err := strconv.Atoi(match[1]); err == nil && n > 0 {
			seconds = n
		}
	}
	// Bound untrusted hints, also keeping auth-looking status numbers out of
	// the public message consumed by text-based error classifiers.
	if seconds > 300 {
		seconds = 300
	}
	return &queueAdmissionError{retryAfterSeconds: seconds, hint: parseQuotaHint(nil, body, cooldownNowFn())}
}
