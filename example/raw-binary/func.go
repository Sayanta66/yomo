package main

import (
	"fmt"

	"github.com/yomorun/yomo/core/rx"
)

// Handler will handle data in Rx way
func Handler(rx rx.Stream) rx.Stream {
	return rx.Subscribe(0x10).
	OnObserve(f).
	StdOut().
	Encode(0x11)
}

var f = func(v []byte) (interface{}, error) {
	return fmt.Sprintf("f.v=%v", v), nil
}
