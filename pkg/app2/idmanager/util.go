package idmanager

import (
	"errors"
	"net"
)

// assertListener asserts that `v` is of type `net.Listener`.
func AssertListener(v interface{}) (net.Listener, error) {
	lis, ok := v.(net.Listener)
	if !ok {
		return nil, errors.New("wrong type of value stored for listener")
	}

	return lis, nil
}

// assertConn asserts that `v` is of type `net.Conn`.
func AssertConn(v interface{}) (net.Conn, error) {
	conn, ok := v.(net.Conn)
	if !ok {
		return nil, errors.New("wrong type of value stored for conn")
	}

	return conn, nil
}
