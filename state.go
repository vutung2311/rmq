package rmq

//go:generate stringer -type=State

type State int

const (
	Unacked State = iota
	Acked
	Delayed
	Rejected
	Pushed
)
