package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCrossOriginBrowserPostsAreRejected(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	body := `{"type":"open","time":"2026-03-10T11:00:00Z","source":"mx-1"}`
	tests := []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"no browser headers", nil, http.StatusAccepted},
		{"same origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusAccepted},
		{"cross site", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"foreign origin", map[string]string{"Origin": "http://203.0.113.7"}, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("POST /v1/events %v = %d, want %d", tc.header, rec.Code, tc.want)
			}
		})
	}
}

func TestReportedErrorsAreBounded(t *testing.T) {
	s, _ := newServer(t)
	tag := strings.Repeat("A", 10_000)
	rec := post(t, s.Handler(), "/v1/events", `{"type":"reply","time":"2026-03-10T11:00:00Z","source":"mx-1","code":451,"tag":"`+tag+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var res IngestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 1 || len(res.Errors[0]) > maxErrorLength {
		t.Errorf("errors = %d entries, first %d bytes, want 1 entry of at most %d bytes", len(res.Errors), len(res.Errors[0]), maxErrorLength)
	}
}

func TestTruncateKeepsUTF8Whole(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"abcdef", 3, "abc"},
		{"abécd", 3, "ab"},
		{"abécd", 4, "abé"},
		{"€€", 4, "€"},
	}
	for _, tc := range tests {
		if got := truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestLimitsFiltersAndHealthz(t *testing.T) {
	s, _ := newServer(t)
	h := s.Handler()
	for _, path := range []string{"/v1/limits/mx-1/gmail", "/v1/limits/mx-1/yahoo", "/v1/limits/warm-1/gmail"} {
		if rec := get(t, h, path); rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
	}
	tests := []struct {
		query string
		want  int
	}{
		{"", 3},
		{"?provider=gmail", 2},
		{"?source=mx-1", 2},
		{"?source=mx-1&provider=yahoo", 1},
		{"?provider=nope", 0},
	}
	for _, tc := range tests {
		var res LimitsResult
		if err := json.Unmarshal(get(t, h, "/v1/limits"+tc.query).Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Lanes) != tc.want {
			t.Errorf("GET /v1/limits%s = %d lanes, want %d", tc.query, len(res.Lanes), tc.want)
		}
	}
	if rec := get(t, h, "/healthz"); rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body)
	}
}

func TestParseSendGridKeyRejectsBadKeys(t *testing.T) {
	for _, key := range []string{"not base64!", "aGVsbG8=", ""} {
		if _, err := ParseSendGridKey(key); err == nil {
			t.Errorf("ParseSendGridKey(%q) = nil error", key)
		}
	}
}
