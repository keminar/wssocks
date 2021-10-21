package pipe

import "time"

type dead struct {
	Line time.Duration
}

func NewDead() *dead {
	return &dead{Line: time.Duration(5) * time.Minute}
}
