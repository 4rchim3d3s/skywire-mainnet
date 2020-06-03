// Code generated by mockery v1.0.0. DO NOT EDIT.

package appevent

import (
	context "context"

	mock "github.com/stretchr/testify/mock"

	appcommon "github.com/SkycoinProject/skywire-mainnet/pkg/app/appcommon"
)

// MockRPCClient is an autogenerated mock type for the RPCClient type
type MockRPCClient struct {
	mock.Mock
}

// Close provides a mock function with given fields:
func (_m *MockRPCClient) Close() error {
	ret := _m.Called()

	var r0 error
	if rf, ok := ret.Get(0).(func() error); ok {
		r0 = rf()
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// Hello provides a mock function with given fields:
func (_m *MockRPCClient) Hello() *appcommon.Hello {
	ret := _m.Called()

	var r0 *appcommon.Hello
	if rf, ok := ret.Get(0).(func() *appcommon.Hello); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*appcommon.Hello)
		}
	}

	return r0
}

// Notify provides a mock function with given fields: ctx, e
func (_m *MockRPCClient) Notify(ctx context.Context, e *Event) error {
	ret := _m.Called(ctx, e)

	var r0 error
	if rf, ok := ret.Get(0).(func(context.Context, *Event) error); ok {
		r0 = rf(ctx, e)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}
