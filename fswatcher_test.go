package fswatcher

import "testing"

func TestOpString(t *testing.T) {
	tests := []struct {
		op   Op
		want string
	}{
		{0, ""},
		{Create, "CREATE"},
		{Write, "WRITE"},
		{Remove, "REMOVE"},
		{Rename, "RENAME"},
		{Chmod, "CHMOD"},
		{Create | Write, "CREATE|WRITE"},
		{Create | Chmod, "CREATE|CHMOD"},
		{All, "CREATE|WRITE|REMOVE|RENAME|CHMOD"},
	}
	for _, tt := range tests {
		if got := tt.op.String(); got != tt.want {
			t.Errorf("Op(%d).String() = %q, want %q", tt.op, got, tt.want)
		}
	}
}

func BenchmarkOpString(b *testing.B) {
	tests := []Op{
		0,
		Create,
		Create | Write,
		Create | Write | Remove,
		Create | Write | Remove | Rename,
		All,
	}

	for _, tt := range tests {
		b.Run(tt.String(), func(b *testing.B) {
			for b.Loop() {
				_ = tt.String()
			}
		})
	}
}

func TestOpHas(t *testing.T) {
	tests := []struct {
		name   string
		op     Op
		target Op
		want   bool
	}{
		{"single bit set", Create, Create, true},
		{"single bit unset", Write, Create, false},
		{"any of multiple bits set", Create, Create | Write, true},
		{"all of multiple bits set", Create | Write, Create | Write, true},
		{"none of multiple bits set", Chmod, Create | Write, false},
		{"superset of target", Create | Write | Chmod, Create | Write, true},
		{"empty op against any", 0, Create, false},
		{"empty target", Create, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.op.Has(tt.target); got != tt.want {
				t.Errorf("Op(%s).Has(%s) = %v, want %v", tt.op, tt.target, got, tt.want)
			}
		})
	}
}
