package refusal

import (
	"errors"
)

type Code int

const (
	CodeUsage Code = 2

	CodeCredential Code = 3

	CodeControlPlane Code = 4

	CodeKubeconfig Code = 5

	CodePermission Code = 6

	CodeWrongCluster Code = 7

	CodeApply Code = 8

	CodeAgentNotReady Code = 9

	CodeNotConfirmed Code = 10
)

type Refusal struct {
	Code   Code
	What   string
	Detail []string
	Next   string
	Cause  error
	Kind   error
}

func (r *Refusal) Error() string {
	return r.What
}

func (r *Refusal) Unwrap() error {
	return r.Cause
}

func (r *Refusal) Is(target error) bool {
	return r.Kind != nil && r.Kind == target
}

func New(code Code, what string, next string) *Refusal {
	return &Refusal{Code: code, What: what, Next: next}
}

func Wrap(code Code, cause error, what string, next string) *Refusal {
	return &Refusal{Code: code, What: what, Next: next, Cause: cause}
}

func (r *Refusal) WithDetail(lines ...string) *Refusal {
	out := *r
	out.Detail = append(append([]string(nil), r.Detail...), lines...)
	return &out
}

func (r *Refusal) WithKind(kind error) *Refusal {
	out := *r
	out.Kind = kind
	return &out
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return int(refusal.Code)
	}
	return 1
}
