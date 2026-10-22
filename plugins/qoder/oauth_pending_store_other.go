//go:build !unix

package main

import "time"

// Explicitly memory-only on non-Unix until an equivalent secure file/locking
// implementation exists. No HOME or configured-auth-dir writes; persistent=false.
func openOAuthPendingStore(string) (*oauthPendingStore, error)      { return nil, nil }
func (*oauthPendingStore) read(string) (*oauthPendingRecord, error) { return nil, errOAuthStorage }
func (*oauthPendingStore) write(*oauthPendingRecord) error          { return errOAuthStorage }
func (*oauthPendingStore) scan(time.Time) (int, error)              { return 0, errOAuthStorage }
func (*oauthPendingStore) remove(string)                            {}
func (*oauthPendingStore) close()                                   {}
