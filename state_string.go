// generated by stringer -type=state; DO NOT EDIT

package queue

import "fmt"

const _state_name = "UnackedAckedRejected"

var _state_index = [...]uint8{0, 7, 12, 20}

func (i state) String() string {
	if i < 0 || i+1 >= state(len(_state_index)) {
		return fmt.Sprintf("state(%d)", i)
	}
	return _state_name[_state_index[i]:_state_index[i+1]]
}
