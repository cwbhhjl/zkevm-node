// Code generated by mockery v2.15.0. DO NOT EDIT.

package mocks

import (
	context "context"

	mock "github.com/stretchr/testify/mock"

	types "github.com/0xPolygonHermez/zkevm-node/etherman/types"
)

// TxmanagerMock is an autogenerated mock type for the txManager type
type TxmanagerMock struct {
	mock.Mock
}

// SequenceBatches provides a mock function with given fields: ctx, sequences
func (_m *TxmanagerMock) SequenceBatches(ctx context.Context, sequences []types.Sequence) error {
	ret := _m.Called(ctx, sequences)

	var r0 error
	if rf, ok := ret.Get(0).(func(context.Context, []types.Sequence) error); ok {
		r0 = rf(ctx, sequences)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

type mockConstructorTestingTNewTxmanagerMock interface {
	mock.TestingT
	Cleanup(func())
}

// NewTxmanagerMock creates a new instance of TxmanagerMock. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
func NewTxmanagerMock(t mockConstructorTestingTNewTxmanagerMock) *TxmanagerMock {
	mock := &TxmanagerMock{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}
