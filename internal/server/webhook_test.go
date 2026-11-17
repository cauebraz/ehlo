package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cauebraz/ehlo/internal/ingest"
	"github.com/cauebraz/ehlo/internal/metrics"
	"github.com/cauebraz/ehlo/internal/provider"
	"github.com/cauebraz/ehlo/internal/warmup"
)

func newWebhookServer(t *testing.T, opts ...Option) http.Handler {
	t.Helper()
	clock := func() time.Time { return now }
	reg := prometheus.NewRegistry()
	cls := provider.New(provider.Default)
	eng, err := warmup.New(warmup.DefaultPolicy(), warmup.Targets{}, cls.Names(), warmup.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New(reg, "test", metrics.WithClock(clock))
	return New(ingest.New(cls, m, eng), eng, reg, append([]Option{WithClock(clock)}, opts...)...).Handler()
}

func postWith(h http.Handler, path, body string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const sendgridBody = `[{"email":"user5@gmail.com","timestamp":1773129601,"event":"delivered"}]`

func TestWebhookToken(t *testing.T) {
	h := newWebhookServer(t, WithWebhookToken("s3cret"))
	for path, want := range map[string]int{
		"/v1/webhooks/sendgrid":              http.StatusUnauthorized,
		"/v1/webhooks/sendgrid?token=wrong":  http.StatusUnauthorized,
		"/v1/webhooks/sendgrid?token=s3cret": http.StatusAccepted,
	} {
		if rec := postWith(h, path, sendgridBody, nil); rec.Code != want {
			t.Errorf("POST %s = %d, want %d", path, rec.Code, want)
		}
	}
}

func TestSendGridSignature(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParseSendGridKey(base64.StdEncoding.EncodeToString(der))
	if err != nil {
		t.Fatal(err)
	}
	h := newWebhookServer(t, WithSendGridKey(pub))
	ts := "1773129601"
	digest := sha256.Sum256([]byte(ts + sendgridBody))
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signed := http.Header{sendGridSignature: {base64.StdEncoding.EncodeToString(sig)}, sendGridTimestamp: {ts}}
	if rec := postWith(h, "/v1/webhooks/sendgrid", sendgridBody, signed); rec.Code != http.StatusAccepted {
		t.Errorf("signed = %d %s, want 202", rec.Code, rec.Body)
	}
	if rec := postWith(h, "/v1/webhooks/sendgrid", strings.Replace(sendgridBody, "gmail", "yahoo", 1), signed); rec.Code != http.StatusUnauthorized {
		t.Errorf("tampered body = %d, want 401", rec.Code)
	}
	if rec := postWith(h, "/v1/webhooks/sendgrid", sendgridBody, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned = %d, want 401", rec.Code)
	}
}

func TestSNSSubscriptionAccepted(t *testing.T) {
	h := newWebhookServer(t)
	body := `{"Type":"SubscriptionConfirmation","TopicArn":"arn:aws:sns:us-east-1:123456789012:ehlo","SubscribeURL":"https://sns.us-east-1.amazonaws.com/?Action=ConfirmSubscription"}`
	if rec := postWith(h, "/v1/webhooks/ses", body, nil); rec.Code != http.StatusOK {
		t.Errorf("subscription confirmation = %d %s, want 200", rec.Code, rec.Body)
	}
}
