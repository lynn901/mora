package evidence

import (
	"strings"
	"testing"

	"github.com/lynn901/mora/internal/domain"
)

func TestRedact_RejectsSecrets(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"api key inline":      `the api_key = "sk-1234567890abcdefghijklmnop1234567890" done`,
		"password assign":     `password=hunter2supersecretvalue123`,
		"aws access key":      `creds AKIAIOSFODNN7EXAMPLE leak`,
		"aws secret":          `aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`,
		"pem private key":     "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAA",
		"github pat":          `deploy token ghp_abcdefghijklmnopqrstuvwxyz0123456789AB`,
		"jwt":                 `bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpXJ`,
		"postgres connstr":    `postgres://mora:s3cr3tvaluepass@host:5432/db`,
		"slack token":         `hook xoxb-1234567890-abcdefghij`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Redact(in)
			if err != ErrSecretDetected {
				t.Fatalf("expected ErrSecretDetected, got %v", err)
			}
		})
	}
}

func TestRedact_MasksPII(t *testing.T) {
	t.Parallel()
	in := `联系 alice@example.com 或电话 13800138000 / +8613800138000; 身份证 110101199003071234; 卡 4111 1111 1111 1111`
	out, err := Redact(in)
	if err != nil {
		t.Fatalf("Redact: unexpected error: %v", err)
	}
	for _, plain := range []string{"alice@example.com", "13800138000", "+8613800138000", "110101199003071234", "4111 1111 1111 1111"} {
		if strings.Contains(out.RedactedText, plain) {
			t.Fatalf("redacted text still contains sensitive span %q: %s", plain, out.RedactedText)
		}
	}
	if out.Classification != domain.EvidenceClassPII {
		t.Fatalf("expected classification=pii, got %q", out.Classification)
	}
	if out.ContentHash == "" {
		t.Fatal("content_hash empty")
	}
}

func TestRedact_CleanInput(t *testing.T) {
	t.Parallel()
	in := `决策：mora-api 默认监听 :8990；工作区隔离走 ltree path。`
	out, err := Redact(in)
	if err != nil {
		t.Fatalf("Redact: unexpected error: %v", err)
	}
	if out.RedactedText != in {
		t.Fatalf("clean input should be unchanged, got %q", out.RedactedText)
	}
	if out.Classification != domain.EvidenceClassNone {
		t.Fatalf("expected classification=none, got %q", out.Classification)
	}
}

func TestRedact_HashStableAfterMasking(t *testing.T) {
	t.Parallel()
	// Two captures of the same logical content (same email) must hash equal —
	// the redacted form, not the raw, is hashed.
	a, _ := Redact(`reach bob@acme.io for the runbook`)
	b, _ := Redact(`reach bob@acme.io for the runbook`)
	if a.ContentHash != b.ContentHash {
		t.Fatal("content_hash not stable for identical redacted input")
	}
	if a.RedactedText != b.RedactedText {
		t.Fatal("redacted text not stable")
	}
}

func TestIsInlineCandidate(t *testing.T) {
	t.Parallel()
	if !IsInlineCandidate(strings.Repeat("x", domain.EvidenceInlineMaxBytes)) {
		t.Fatal("fragment at the ceiling should be inline")
	}
	if IsInlineCandidate(strings.Repeat("x", domain.EvidenceInlineMaxBytes+1)) {
		t.Fatal("fragment above the ceiling should route to object storage")
	}
}

func TestExcerpt_RuneCounted(t *testing.T) {
	t.Parallel()
	// CJK multibyte — must not split a sequence.
	in := strings.Repeat("语", 300)
	got := Excerpt(in, 10)
	if strings.Count(got, "语") != 10 {
		t.Fatalf("excerpt truncated to wrong rune count: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("excerpt should end with ellipsis: %q", got)
	}
	// Short input returned whole.
	short := "短片段"
	if got := Excerpt(short, 256); got != short {
		t.Fatalf("short input should be returned whole, got %q", got)
	}
}
