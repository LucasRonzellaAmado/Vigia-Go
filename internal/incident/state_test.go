package incident

import (
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	t0 := time.Date(2026, 10, 2, 13, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)

	tests := []struct {
		name      string
		state     State
		ok        bool
		threshold int
		want      State
		wantTr    Transition
	}{
		{
			name:   "primeiro sucesso sai de unknown sem alerta",
			state:  State{Status: StatusUnknown},
			ok:     true,
			want:   State{Status: StatusUp},
			wantTr: NoChange,
		},
		{
			name:      "uma falha isolada não derruba (falso positivo)",
			state:     State{Status: StatusUp},
			ok:        false,
			threshold: 3,
			want:      State{Status: StatusUp, ConsecutiveFailures: 1, FailingSince: t1},
			wantTr:    NoChange,
		},
		{
			name:      "segunda falha ainda abaixo do limite mantém o início da sequência",
			state:     State{Status: StatusUp, ConsecutiveFailures: 1, FailingSince: t0},
			ok:        false,
			threshold: 3,
			want:      State{Status: StatusUp, ConsecutiveFailures: 2, FailingSince: t0},
			wantTr:    NoChange,
		},
		{
			name:      "atingiu N falhas seguidas: caiu",
			state:     State{Status: StatusUp, ConsecutiveFailures: 2, FailingSince: t0},
			ok:        false,
			threshold: 3,
			want:      State{Status: StatusDown, ConsecutiveFailures: 3, FailingSince: t0},
			wantTr:    WentDown,
		},
		{
			name:      "sucesso no meio da sequência zera o contador",
			state:     State{Status: StatusUp, ConsecutiveFailures: 2, FailingSince: t0},
			ok:        true,
			threshold: 3,
			want:      State{Status: StatusUp},
			wantTr:    NoChange,
		},
		{
			name:      "já caído e continua falhando: sem nova transição",
			state:     State{Status: StatusDown, ConsecutiveFailures: 5, FailingSince: t0},
			ok:        false,
			threshold: 3,
			want:      State{Status: StatusDown, ConsecutiveFailures: 6, FailingSince: t0},
			wantTr:    NoChange,
		},
		{
			name:      "caído e voltou",
			state:     State{Status: StatusDown, ConsecutiveFailures: 6, FailingSince: t0},
			ok:        true,
			threshold: 3,
			want:      State{Status: StatusUp},
			wantTr:    Recovered,
		},
		{
			name:      "threshold 1 derruba na primeira falha",
			state:     State{Status: StatusUnknown},
			ok:        false,
			threshold: 1,
			want:      State{Status: StatusDown, ConsecutiveFailures: 1, FailingSince: t1},
			wantTr:    WentDown,
		},
		{
			name:      "threshold inválido vira 1",
			state:     State{Status: StatusUp},
			ok:        false,
			threshold: 0,
			want:      State{Status: StatusDown, ConsecutiveFailures: 1, FailingSince: t1},
			wantTr:    WentDown,
		},
		{
			name:      "monitor novo falhando desde o início também cai",
			state:     State{Status: StatusUnknown, ConsecutiveFailures: 1, FailingSince: t0},
			ok:        false,
			threshold: 2,
			want:      State{Status: StatusDown, ConsecutiveFailures: 2, FailingSince: t0},
			wantTr:    WentDown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, tr := Evaluate(tt.state, tt.ok, t1, tt.threshold)
			if got != tt.want {
				t.Errorf("estado = %+v, queria %+v", got, tt.want)
			}
			if tr != tt.wantTr {
				t.Errorf("transição = %v, queria %v", tr, tt.wantTr)
			}
		})
	}
}

func TestEvaluateSequence(t *testing.T) {
	start := time.Date(2026, 10, 2, 13, 0, 0, 0, time.UTC)
	checks := []bool{true, false, true, false, false, false, false, true, true}
	wantTr := []Transition{NoChange, NoChange, NoChange, NoChange, NoChange, WentDown, NoChange, Recovered, NoChange}

	s := State{Status: StatusUnknown}
	for i, ok := range checks {
		var tr Transition
		s, tr = Evaluate(s, ok, start.Add(time.Duration(i)*time.Minute), 3)
		if tr != wantTr[i] {
			t.Fatalf("checagem %d: transição = %v, queria %v", i, tr, wantTr[i])
		}
	}
	if s.Status != StatusUp || s.ConsecutiveFailures != 0 {
		t.Fatalf("estado final = %+v", s)
	}
}
