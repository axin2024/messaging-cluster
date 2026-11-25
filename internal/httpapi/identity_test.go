package httpapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/axin2024/messaging-cluster/internal/credential"
	"github.com/axin2024/messaging-cluster/internal/store"
)

func TestIdentityOnMySQL(t *testing.T) {
	dsn := testMySQLDSN(t)
	for range 2 {
		if version, err := store.Migrate(dsn); err != nil || version != store.SchemaVersion {
			t.Fatalf("migrate schema: version %d, error %v", version, err)
		}
	}
	storage, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	handler, err := New(storage, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f := identityFixture{handler: handler}
	f.check(t, "GET", "/readyz", "", nil, http.StatusOK, nil)

	adminToken, digest, err := credential.IssueToken()
	if err != nil {
		t.Fatal(err)
	}
	tenant := "harbor-" + hex.EncodeToString(digest[:6])
	primary, err := storage.CreateApp(context.Background(), tenant, "dispatch", digest)
	if err != nil {
		t.Fatal(err)
	}
	outsideAdmin, outsideDigest, err := credential.IssueToken()
	if err != nil {
		t.Fatal(err)
	}
	outsideTenant := "outside-" + hex.EncodeToString(outsideDigest[:6])
	outside, err := storage.CreateApp(context.Background(), outsideTenant, "dispatch", outsideDigest)
	if err != nil {
		t.Fatal(err)
	}
	const alicePassword = "synthetic-alice-password"
	const outsidePassword = "synthetic-outside-password"
	user := map[string]string{"username": "alice", "display_name": "调度操作员", "password": alicePassword}
	var alice, other store.Account
	f.check(t, "POST", "/v1/admin/users", adminToken, user, http.StatusCreated, &alice)
	f.check(t, "POST", "/v1/admin/users", adminToken, user, http.StatusConflict, nil)
	user["password"] = outsidePassword
	f.check(t, "POST", "/v1/admin/users", outsideAdmin, user, http.StatusCreated, &other)
	if alice.Scope != primary || other.Scope != outside || alice.UserID == other.UserID {
		t.Fatal("account scope mismatch")
	}
	login := func(tenant, password, device string) map[string]string {
		return map[string]string{"tenant": tenant, "app": "dispatch", "username": "alice", "password": password, "device_key": device}
	}
	var mobile, desktop, otherMobile sessionResponse
	f.check(t, "POST", "/v1/sessions", "", login(tenant, alicePassword, "phone"), http.StatusCreated, &mobile)
	f.check(t, "POST", "/v1/sessions", "", login(tenant, alicePassword, "desktop"), http.StatusCreated, &desktop)
	f.check(t, "POST", "/v1/sessions", "", login(outsideTenant, alicePassword, "phone"), http.StatusUnauthorized, nil)
	f.check(t, "POST", "/v1/sessions", "", login(outsideTenant, outsidePassword, "phone"), http.StatusCreated, &otherMobile)

	f.check(t, "GET", "/v1/me", adminToken, nil, http.StatusUnauthorized, nil)
	f.check(t, "POST", "/v1/admin/users", mobile.Token, user, http.StatusUnauthorized, nil)
	f.check(t, "DELETE", fmt.Sprintf("/v1/devices/%d", otherMobile.Session.DeviceID), desktop.Token, nil, http.StatusNotFound, nil)
	f.check(t, "GET", "/v1/me", otherMobile.Token, nil, http.StatusOK, nil)
	var listed struct {
		Items []store.Device `json:"items"`
	}
	f.check(t, "GET", "/v1/devices?tenant="+outsideTenant, desktop.Token, nil, http.StatusOK, &listed)
	if len(listed.Items) != 2 || listed.Items[0].DeviceID != mobile.Session.DeviceID || listed.Items[1].DeviceID != desktop.Session.DeviceID {
		t.Fatal("bearer scope did not fence device listing")
	}
	foreignUser := map[string]string{"username": "mallory", "display_name": "Synthetic", "password": outsidePassword, "tenant": outsideTenant}
	f.check(t, "POST", "/v1/admin/users", adminToken, foreignUser, http.StatusBadRequest, nil)

	var rotated sessionResponse
	f.check(t, "POST", "/v1/devices/current/rotate", desktop.Token, nil, http.StatusOK, &rotated)
	if rotated.Session.DeviceID != desktop.Session.DeviceID || rotated.Token == desktop.Token {
		t.Fatal("rotation failed to keep device identity and replace credentials")
	}
	f.check(t, "GET", "/v1/me", desktop.Token, nil, http.StatusUnauthorized, nil)
	f.check(t, "GET", "/v1/me", mobile.Token, nil, http.StatusOK, nil)
	revokePath := fmt.Sprintf("/v1/devices/%d", mobile.Session.DeviceID)
	f.check(t, "DELETE", revokePath, rotated.Token, nil, http.StatusNoContent, nil)
	f.check(t, "DELETE", revokePath, rotated.Token, nil, http.StatusNoContent, nil)
	f.check(t, "GET", "/v1/me", mobile.Token, nil, http.StatusUnauthorized, nil)
	f.check(t, "GET", "/v1/me", rotated.Token, nil, http.StatusOK, nil)

	var relogin sessionResponse
	f.check(t, "POST", "/v1/sessions", "", login(tenant, alicePassword, "phone"), http.StatusCreated, &relogin)
	if relogin.Session.DeviceID != mobile.Session.DeviceID {
		t.Fatal("relogin changed the device identity")
	}
	f.check(t, "GET", "/v1/me", relogin.Token, nil, http.StatusOK, nil)
	f.check(t, "GET", "/v1/me", mobile.Token, nil, http.StatusUnauthorized, nil)

	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			<-start
			responses <- f.request("POST", "/v1/devices/current/rotate", relogin.Token, nil)
		}()
	}
	close(start)
	successes := 0
	var winner sessionResponse
	for range 2 {
		response := <-responses
		switch response.Code {
		case http.StatusOK:
			successes++
			if err := json.Unmarshal(response.Body.Bytes(), &winner); err != nil {
				t.Fatal(err)
			}
		case http.StatusUnauthorized:
		default:
			t.Fatalf("unexpected concurrent rotation status %d", response.Code)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent rotation had %d successful requests", successes)
	}
	f.check(t, "GET", "/v1/me", relogin.Token, nil, http.StatusUnauthorized, nil)
	f.check(t, "GET", "/v1/me", winner.Token, nil, http.StatusOK, nil)
}

type identityFixture struct {
	handler http.Handler
}

func testMySQLDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MESSAGING_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set MESSAGING_TEST_MYSQL_DSN for MySQL integration")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil || !strings.HasPrefix(config.DBName, "messaging_test_") {
		t.Fatal("use an isolated MySQL database named messaging_test_*")
	}
	return dsn
}

func (f identityFixture) check(t *testing.T, method, path, token string, payload any, status int, result any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	recorder := f.request(method, path, token, bytes.NewReader(body))
	if recorder.Code != status || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%s %s: got status %d, want %d", method, path, recorder.Code, status)
	}
	if result != nil {
		if err := json.Unmarshal(recorder.Body.Bytes(), result); err != nil {
			t.Fatal(err)
		}
	}
}

func (f identityFixture) request(method, path, token string, body io.Reader) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}
