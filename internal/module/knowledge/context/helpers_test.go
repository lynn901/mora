package context

// helpers_test.go holds small shared test helpers for the context-package unit
// tests. Kept in one file so the per-component test files stay focused on the
// §4/§5/§6/§8 branches they pin.

import "github.com/google/uuid"

// id returns a deterministic-ish uuid from a name seed. Not cryptographically
// meaningful — just stable across a test so candidate identity is legible in
// failure output (id("doc") reads better than uuid.New() in an assertion
// message).
func id(name string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceDNS, []byte("mora-context-test:"+name))
}
