package identity

import (
	"crypto/rand"
	"errors"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var projectIDPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func TestFreeTierRequestIDsKeepsGatewayIdentityAndReusesRetries(t *testing.T) {
	t.Setenv("OPENCODE_ZEN_FREE_SESSION_AFFINITY", "")
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("x-opencode-session", "client-conversation")
	r.Header.Set("x-opencode-session-id", "child-conversation")
	r.Header.Set("x-parent-session-id", "parent-conversation")
	first := DeriveRequestIDs(r, nil, "caller")
	second := DeriveRequestIDs(r, nil, "caller")
	if first.Session != second.Session || first.Project != second.Project {
		t.Fatal("stable gateway routing identities changed between matching client requests")
	}
	outbound := FreeTierRequestIDs(first)
	other := FreeTierRequestIDs(second)
	if outbound.Session == first.Session || outbound.Session == other.Session {
		t.Fatal("anonymous requests did not receive separate fresh sessions")
	}
	if outbound.Project == other.Project || !projectIDPattern.MatchString(outbound.Project) {
		t.Fatalf("unnamed request projects were not private git-hash shaped IDs: %q, %q", outbound.Project, other.Project)
	}
	if outbound.Session != outbound.ChildSession || outbound.ParentSession != "" {
		t.Fatal("anonymous request retained a different child or parent session")
	}
	if outbound.Request != first.Request || outbound.Message != first.Message {
		t.Fatal("anonymous shaping changed gateway request/message correlation")
	}
	if retry := FreeTierRequestIDs(first); retry != outbound {
		t.Fatal("retries of one incoming request changed the outbound identity")
	}
	if repeat := FreeTierRequestIDs(outbound); repeat != outbound {
		t.Fatal("applying anonymous identity shaping twice changed its result")
	}
	if first.Session == first.FreeTierSession || first.Project == first.FreeTierProject || first.ParentSession == "" {
		t.Fatal("anonymous shaping mutated the original gateway identity")
	}
}

func TestFreeTierRequestIDsPreservesProvidedProject(t *testing.T) {
	t.Setenv("OPENCODE_ZEN_FREE_SESSION_AFFINITY", "")
	for _, tt := range []struct {
		name, header, metadata, want string
	}{
		{name: "header", header: strings.Repeat("a", 40), want: strings.Repeat("a", 40)},
		{name: "metadata", metadata: "host-project", want: "host-project"},
		{name: "header wins", header: "header-project", metadata: "body-project", want: "header-project"},
		{name: "explicit name resembling internal default", header: "opencode2api:default-project:host", want: "opencode2api:default-project:host"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			if tt.header != "" {
				r.Header.Set("x-opencode-project", tt.header)
			}
			body := map[string]any{"metadata": map[string]any{"project_id": tt.metadata}}
			ids := DeriveRequestIDs(r, body, "caller")
			if !ids.ProjectProvided || ids.FreeTierProject != tt.want {
				t.Fatalf("caller project was not retained: %+v", ids)
			}
			if got := FreeTierRequestIDs(ids).Project; got != tt.want {
				t.Fatalf("outbound project = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFreeTierSessionAffinity(t *testing.T) {
	t.Setenv("OPENCODE_ZEN_FREE_SESSION_AFFINITY", "1")
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("x-opencode-session", "same-conversation")
	r.Header.Set("x-opencode-session-id", "a-child")
	first := DeriveRequestIDs(r, nil, "caller")
	second := DeriveRequestIDs(r, nil, "caller")
	a, b := FreeTierRequestIDs(first), FreeTierRequestIDs(second)
	if a.Session != first.Session || a.Session != b.Session || a.ChildSession != a.Session {
		t.Fatal("session affinity did not retain the stable conversation in both headers")
	}
	if a.Project == b.Project || !projectIDPattern.MatchString(a.Project) {
		t.Fatal("session affinity restored the shared internal default project")
	}
}

func TestFreeTierRequestIDsFillsManuallyConstructedRequests(t *testing.T) {
	t.Setenv("OPENCODE_ZEN_FREE_SESSION_AFFINITY", "")
	for _, project := range []string{"", "opencode2api:default-project:caller", "probe-project"} {
		ids := FreeTierRequestIDs(RequestIDs{Request: "req_probe", Project: project})
		if !isOfficialSessionID(ids.Session) || ids.Session != ids.ChildSession {
			t.Fatalf("probe session is not OpenCode-shaped: %q", ids.Session)
		}
		if project == "probe-project" {
			if ids.Project != project {
				t.Fatal("explicit probe project was changed")
			}
		} else if !projectIDPattern.MatchString(ids.Project) {
			t.Fatalf("unnamed probe project = %q", ids.Project)
		}
		if repeat := FreeTierRequestIDs(ids); repeat != ids {
			t.Fatal("filled probe IDs were not reusable")
		}
	}
}

func TestFreshSessionIDUsesCurrentTimeAndSmallCounter(t *testing.T) {
	now := time.Now()
	wantTime := uint64(now.UnixMilli()) & ((1 << 36) - 1)
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := freshSessionID(now)
		if !isOfficialSessionID(id) || seen[id] {
			t.Fatalf("duplicate or invalid fresh session %q", id)
		}
		seen[id] = true
		encoded, err := strconv.ParseUint(id[4:16], 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		word := ^encoded & 0xffffffffffff
		if word>>12 != wantTime || word&0xfff < 1 || word&0xfff > 15 {
			t.Fatalf("session time/counter does not describe the current request: %q", id)
		}
	}
}

func TestFreshSessionIDConcurrentUniqueness(t *testing.T) {
	const requests = 512
	ids := make(chan string, requests)
	var workers sync.WaitGroup
	for i := 0; i < requests; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ids <- freshSessionID(time.Now())
		}()
	}
	workers.Wait()
	close(ids)
	seen := make(map[string]bool)
	for id := range ids {
		if seen[id] || !isOfficialSessionID(id) {
			t.Fatalf("concurrent session was duplicated or malformed: %q", id)
		}
		seen[id] = true
	}
}

type failedEntropyReader struct{}

func (failedEntropyReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy unavailable")
}

func TestFreeTierIdentityEntropyFailureDoesNotShareIDs(t *testing.T) {
	t.Setenv("OPENCODE_ZEN_FREE_SESSION_AFFINITY", "")
	previous := rand.Reader
	rand.Reader = failedEntropyReader{}
	t.Cleanup(func() { rand.Reader = previous })
	sessions, projects := make(map[string]bool), make(map[string]bool)
	for i := 0; i < 16; i++ {
		ids := FreeTierRequestIDs(RequestIDs{})
		if !isOfficialSessionID(ids.Session) || !projectIDPattern.MatchString(ids.Project) || sessions[ids.Session] || projects[ids.Project] {
			t.Fatal("entropy failure produced an invalid or shared identity")
		}
		sessions[ids.Session], projects[ids.Project] = true, true
	}
}
