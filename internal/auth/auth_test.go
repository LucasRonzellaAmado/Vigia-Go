package auth

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luquinha/vigia/internal/store"
)

func TestSlugify(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Meu Projeto", "meu-projeto"},
		{"  Ação & Reação!  ", "acao-reacao"},
		{"lucas.ronzella", "lucas-ronzella"},
		{"---", ""},
		{strings.Repeat("a", 60), strings.Repeat("a", 40)},
	}
	for _, tt := range tests {
		if got := Slugify(tt.in); got != tt.want {
			t.Errorf("Slugify(%q) = %q, queria %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{" Lucas@Exemplo.com ", "lucas@exemplo.com", false},
		{"sem-arroba", "", true},
		{"Fulano <f@x.com>", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := NormalizeEmail(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("NormalizeEmail(%q) = %q, %v", tt.in, got, err)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		pw      string
		wantErr bool
	}{
		{"curta", true},
		{"senhaboa", false},
		{strings.Repeat("x", 73), true},
	}
	for _, tt := range tests {
		if err := ValidatePassword(tt.pw); (err != nil) != tt.wantErr {
			t.Errorf("ValidatePassword(len %d) = %v", len(tt.pw), err)
		}
	}
}

func TestTokenIsHashedBeforeStorage(t *testing.T) {
	token, hash, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 40 {
		t.Errorf("token curto demais: %d", len(token))
	}
	if !bytes.Equal(hash, hashToken(token)) {
		t.Error("hash não bate")
	}
	if bytes.Contains(hash, []byte(token)) {
		t.Error("hash não deveria conter o token")
	}
}

func TestLimitsFor(t *testing.T) {
	free, pro, unknown := LimitsFor("free"), LimitsFor("pro"), LimitsFor("enterprise-do-futuro")
	if free.MaxMonitors >= pro.MaxMonitors || free.MinIntervalSeconds <= pro.MinIntervalSeconds {
		t.Errorf("pro deveria ser mais generoso: free=%+v pro=%+v", free, pro)
	}
	if unknown != free {
		t.Errorf("plano desconhecido deveria cair no free: %+v", unknown)
	}
}

func TestRequireUser(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := RequireUser(ok)

	tests := []struct {
		name       string
		path       string
		htmx       bool
		user       bool
		wantStatus int
		wantHeader [2]string
	}{
		{"html sem login redireciona", "/", false, false, http.StatusSeeOther, [2]string{"Location", "/login"}},
		{"api sem login dá 401", "/api/v1/monitors", false, false, http.StatusUnauthorized, [2]string{"Content-Type", "application/json"}},
		{"htmx sem login usa HX-Redirect", "/monitors/1/results", true, false, http.StatusUnauthorized, [2]string{"HX-Redirect", "/login"}},
		{"logado passa", "/", false, true, http.StatusTeapot, [2]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.htmx {
				r.Header.Set("HX-Request", "true")
			}
			if tt.user {
				r = r.WithContext(WithUser(r.Context(), store.User{ID: 1}))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, queria %d", w.Code, tt.wantStatus)
			}
			if tt.wantHeader[0] != "" && w.Header().Get(tt.wantHeader[0]) != tt.wantHeader[1] {
				t.Errorf("%s = %q", tt.wantHeader[0], w.Header().Get(tt.wantHeader[0]))
			}
		})
	}
}
