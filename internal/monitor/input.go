package monitor

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/luquinha/vigia/internal/auth"
	"github.com/luquinha/vigia/internal/checker"
	"github.com/luquinha/vigia/internal/store"
)

type Input struct {
	ProjectID        int64  `json:"project_id"`
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	Target           string `json:"target"`
	IntervalSeconds  int    `json:"interval_seconds"`
	TimeoutSeconds   int    `json:"timeout_seconds"`
	Retries          *int   `json:"retries"`
	FailureThreshold int    `json:"failure_threshold"`
	ExpectedStatus   int    `json:"expected_status"`
	Keyword          string `json:"keyword"`
	SSLExpiryDays    int    `json:"ssl_expiry_days"`
	Enabled          *bool  `json:"enabled"`
}

type ValidationErrors map[string]string

func (v ValidationErrors) Error() string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+v[k])
	}
	return "dados inválidos: " + strings.Join(parts, "; ")
}

func (in *Input) Normalize() {
	in.Name = strings.TrimSpace(in.Name)
	in.Kind = strings.ToLower(strings.TrimSpace(in.Kind))
	in.Target = strings.TrimSpace(in.Target)
	in.Keyword = strings.TrimSpace(in.Keyword)

	if in.IntervalSeconds == 0 {
		in.IntervalSeconds = 60
		switch in.Kind {
		case string(checker.KindSSL):
			in.IntervalSeconds = 12 * 3600
		case string(checker.KindHeartbeat):
			in.IntervalSeconds = 3600
		}
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = 10
		if in.Kind == string(checker.KindHeartbeat) {
			in.TimeoutSeconds = 300
		}
	}
	if in.Retries == nil {
		r := 1
		if in.Kind == string(checker.KindHeartbeat) {
			r = 0
		}
		in.Retries = &r
	}
	if in.FailureThreshold == 0 {
		in.FailureThreshold = 3
		if in.Kind == string(checker.KindHeartbeat) || in.Kind == string(checker.KindSSL) {
			in.FailureThreshold = 1
		}
	}
	if in.SSLExpiryDays == 0 {
		in.SSLExpiryDays = 14
	}
	if in.Enabled == nil {
		e := true
		in.Enabled = &e
	}
	if in.Kind == string(checker.KindHeartbeat) {
		in.Target = ""
	}
	if in.Kind == string(checker.KindHTTP) && in.Target != "" && !strings.Contains(in.Target, "://") {
		in.Target = "https://" + in.Target
	}
}

func (in Input) Validate(l auth.Limits) ValidationErrors {
	errs := ValidationErrors{}
	kind := checker.Kind(in.Kind)

	if in.ProjectID <= 0 {
		errs["project_id"] = "escolha um projeto"
	}
	if n := utf8.RuneCountInString(in.Name); n == 0 || n > 100 {
		errs["name"] = "nome obrigatório (até 100 caracteres)"
	}
	if !kind.Valid() {
		errs["kind"] = "tipo inválido (http, tcp, ping, ssl ou heartbeat)"
	} else if msg := validateTarget(kind, in.Target); msg != "" {
		errs["target"] = msg
	}

	if in.IntervalSeconds < l.MinIntervalSeconds {
		errs["interval_seconds"] = fmt.Sprintf("mínimo de %ds no plano %s", l.MinIntervalSeconds, l.Plan.Label())
	} else if in.IntervalSeconds > 7*24*3600 {
		errs["interval_seconds"] = "máximo de 7 dias"
	}

	if kind == checker.KindHeartbeat {
		if in.TimeoutSeconds < 1 || in.TimeoutSeconds > 86400 {
			errs["timeout_seconds"] = "tolerância entre 1s e 24h"
		}
	} else if in.TimeoutSeconds < 1 || in.TimeoutSeconds > 60 {
		errs["timeout_seconds"] = "timeout entre 1 e 60 segundos"
	} else if in.TimeoutSeconds >= in.IntervalSeconds {
		errs["timeout_seconds"] = "timeout precisa ser menor que o intervalo"
	}

	if in.Retries != nil && (*in.Retries < 0 || *in.Retries > 5) {
		errs["retries"] = "entre 0 e 5"
	}
	if in.FailureThreshold < 1 || in.FailureThreshold > 20 {
		errs["failure_threshold"] = "entre 1 e 20"
	}
	if in.ExpectedStatus != 0 && (in.ExpectedStatus < 100 || in.ExpectedStatus > 599) {
		errs["expected_status"] = "use 0 (qualquer 2xx/3xx) ou um status entre 100 e 599"
	}
	if utf8.RuneCountInString(in.Keyword) > 200 {
		errs["keyword"] = "até 200 caracteres"
	}
	if in.SSLExpiryDays < 1 || in.SSLExpiryDays > 365 {
		errs["ssl_expiry_days"] = "entre 1 e 365 dias"
	}

	if len(errs) == 0 {
		return nil
	}
	return errs
}

func validateTarget(kind checker.Kind, target string) string {
	switch kind {
	case checker.KindHTTP:
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "informe uma URL http(s) válida, ex.: https://meusite.com/health"
		}
	case checker.KindTCP:
		host, port, err := net.SplitHostPort(target)
		if err != nil || host == "" {
			return "use host:porta, ex.: db.meusite.com:5432"
		}
		if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			return "porta inválida"
		}
	case checker.KindPing:
		if target == "" || strings.ContainsAny(target, "/: ") {
			return "informe só o host ou IP, ex.: 1.1.1.1"
		}
	case checker.KindSSL:
		if target == "" || strings.Contains(target, " ") {
			return "informe o domínio, ex.: meusite.com ou meusite.com:8443"
		}
		if strings.Contains(target, "://") {
			u, err := url.Parse(target)
			if err != nil || u.Scheme != "https" || u.Host == "" {
				return "use https://dominio ou só o domínio"
			}
		}
	}
	return ""
}

func InputFromForm(r *http.Request) Input {
	atoi := func(k string) int {
		n, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue(k)))
		return n
	}
	projectID, _ := strconv.ParseInt(r.PostFormValue("project_id"), 10, 64)
	retries := atoi("retries")
	enabled := r.PostFormValue("enabled") != ""

	return Input{
		ProjectID:        projectID,
		Name:             r.PostFormValue("name"),
		Kind:             r.PostFormValue("kind"),
		Target:           r.PostFormValue("target"),
		IntervalSeconds:  atoi("interval_seconds"),
		TimeoutSeconds:   atoi("timeout_seconds"),
		Retries:          &retries,
		FailureThreshold: atoi("failure_threshold"),
		ExpectedStatus:   atoi("expected_status"),
		Keyword:          r.PostFormValue("keyword"),
		SSLExpiryDays:    atoi("ssl_expiry_days"),
		Enabled:          &enabled,
	}
}

func InputFromMonitor(m store.Monitor) Input {
	retries := int(m.Retries)
	enabled := m.Enabled
	return Input{
		ProjectID:        m.ProjectID,
		Name:             m.Name,
		Kind:             m.Kind,
		Target:           m.Target,
		IntervalSeconds:  int(m.IntervalSeconds),
		TimeoutSeconds:   int(m.TimeoutSeconds),
		Retries:          &retries,
		FailureThreshold: int(m.FailureThreshold),
		ExpectedStatus:   int(m.ExpectedStatus),
		Keyword:          m.Keyword,
		SSLExpiryDays:    int(m.SslExpiryDays),
		Enabled:          &enabled,
	}
}

func DefaultInput(kind string, projectID int64) Input {
	in := Input{Kind: kind, ProjectID: projectID}
	if in.Kind == "" {
		in.Kind = string(checker.KindHTTP)
	}
	in.Normalize()
	return in
}

func TargetOf(m store.Monitor) checker.Target {
	t := checker.Target{
		Kind:           checker.Kind(m.Kind),
		Address:        m.Target,
		Timeout:        time.Duration(m.TimeoutSeconds) * time.Second,
		Retries:        int(m.Retries),
		ExpectedStatus: int(m.ExpectedStatus),
		Keyword:        m.Keyword,
		SSLExpiryDays:  int(m.SslExpiryDays),
		Interval:       time.Duration(m.IntervalSeconds) * time.Second,
		CreatedAt:      m.CreatedAt,
	}
	if m.LastHeartbeatAt != nil {
		t.LastHeartbeat = *m.LastHeartbeatAt
	}
	return t
}
