package redact

import (
	"strings"
	"testing"
)

// The values below are syntactically valid and cryptographically worthless:
// they are structured to match the rules and are not credentials for anything.
const (
	fakeAWSKey  = "AKIAIOSFODNN7EXAMPLE"
	fakeSK      = "sk-abcdefghijklmnopqrstuvwxyz0123456789ABCD"
	fakeAntSK   = "sk-ant-api03-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fakeJWT     = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	fakeGitHub  = "ghp_1234567890abcdefghijklmnopqrstuvwxyzAB"
	fakeGoogle  = "AIzaSyA1234567890abcdefghijklmnopqrstuv"
	fakePEMBody = "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\nabcd\n-----END RSA PRIVATE KEY-----"
)

func TestApplyRemovesEverySecretClass(t *testing.T) {
	cases := []struct {
		name  string
		input string
		leak  string
		want  Kind
	}{
		{"aws access key", "deploy uses " + fakeAWSKey + " for s3", fakeAWSKey, KindAWSAccessKey},
		{"openai key", "OPENAI_API_KEY=" + fakeSK, fakeSK, KindOpenAIKey},
		{"anthropic key", "export ANTHROPIC_API_KEY=" + fakeAntSK, fakeAntSK, KindAnthropicKey},
		{"jwt", "curl -H 'Authorization: Bearer " + fakeJWT + "'", fakeJWT, KindJWT},
		{"pem block", "key material:\n" + fakePEMBody, "MIIEowIBAAKCAQEA1234", KindPrivateKey},
		{"github token", "git remote set-url origin https://" + fakeGitHub + "@github.com/a/b", fakeGitHub, KindGitHubToken},
		{"google api key", "maps key " + fakeGoogle, fakeGoogle, KindGoogleKey},
		{"password assignment", "password=hunter2000secret", "hunter2000secret", KindPassword},
		{"connection string", "postgres://dkm:s3cr3tp4ss@10.0.0.4:5432/dkm", "s3cr3tp4ss", KindConnString},
		{"dkm key", "logged in with pmk_a3f2_ZZZZfakefakefake0000", "pmk_a3f2_ZZZZfakefakefake0000", KindDKMKey},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, findings := Apply(tc.input)
			if len(findings) == 0 {
				t.Fatalf("no finding for %s; output was %q", tc.name, out)
			}
			if strings.Contains(out, tc.leak) {
				t.Fatalf("secret survived redaction: %q still contains %q", out, tc.leak)
			}
			var kinds []string
			hit := false
			for _, f := range findings {
				kinds = append(kinds, string(f.Kind))
				if f.Kind == tc.want {
					hit = true
				}
			}
			if !hit {
				t.Fatalf("expected kind %q, got %v", tc.want, kinds)
			}
			if !strings.Contains(out, "[redacted:") {
				t.Fatalf("expected a redaction marker in %q", out)
			}
		})
	}
}

func TestFindingsCarryNoSecretText(t *testing.T) {
	// A Finding is printed in dry-run reports and written to logs. If it ever
	// gains a field holding the matched text, that is a leak by construction,
	// so the guarantee is asserted here rather than left to review.
	_, findings := Apply("password=hunter2000secret and " + fakeAWSKey)
	if len(findings) < 2 {
		t.Fatalf("expected 2 findings, got %d", len(findings))
	}
	for _, f := range findings {
		if f.Length <= 0 {
			t.Errorf("finding %v has no length", f)
		}
		if f.Line < 1 || f.Column < 1 {
			t.Errorf("finding %v has a zero-based position", f)
		}
	}
}

func TestOrdinaryProseSurvivesUntouched(t *testing.T) {
	// Over-eager redaction is its own failure: memories full of
	// [redacted:generic_secret] are as useless as no memories.
	clean := []string{
		"cloudflared runs under PM2 here, not systemd",
		"we chose jose over jsonwebtoken for Edge runtime compatibility",
		"the deploy needs Node 20, not 22",
		"see https://github.com/IzE-PewPewPew/DK-AgentMemory/blob/main/docs/API.md",
		"run: SELECT id, title FROM memories WHERE project = $1 ORDER BY created_at DESC",
		"the password reset flow sends an email, it does not expire the session",
	}
	for _, s := range clean {
		if out, f := Apply(s); out != s {
			t.Errorf("ordinary text was redacted:\n  in:  %q\n  out: %q\n  by:  %v", s, out, f)
		}
	}
}

func TestOverlappingMatchesDoNotCorruptOutput(t *testing.T) {
	// A connection string whose password is itself token-shaped matches several
	// rules over the same bytes. Nested replacement would corrupt offsets.
	in := "DATABASE_URL=postgres://user:" + fakeSK + "@db.internal:5432/app"
	out, findings := Apply(in)
	if strings.Contains(out, fakeSK) {
		t.Fatalf("secret survived: %q", out)
	}
	if n := strings.Count(out, "[redacted:"); n != len(findings) {
		t.Fatalf("marker count %d does not match finding count %d in %q", n, len(findings), out)
	}
	for i := 1; i < len(findings); i++ {
		if findings[i].Offset < findings[i-1].Offset+findings[i-1].Length {
			t.Fatalf("findings overlap: %+v then %+v", findings[i-1], findings[i])
		}
	}
}

func TestLinePositions(t *testing.T) {
	in := "line one\nline two\nkey " + fakeAWSKey + "\n"
	findings := Scan(in)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Line != 3 {
		t.Errorf("line: got %d, want 3", findings[0].Line)
	}
	if findings[0].Column != 5 {
		t.Errorf("column: got %d, want 5", findings[0].Column)
	}
}

func TestEmptyAndNoMatch(t *testing.T) {
	if out, f := Apply(""); out != "" || f != nil {
		t.Errorf("empty input: got %q %v", out, f)
	}
	if Has("nothing to see here") {
		t.Error("false positive on plain text")
	}
}
