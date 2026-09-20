// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"fmt"
	"sync"
)

// SecurityGroupManager holds the security group applied to the ENIs created for
// pods. Today it is set once from the config at daemon start and then read on
// the ENI creation path, so implementations must be safe for concurrent use.
type SecurityGroupManager interface {
	GetSecurityGroup() string
	UpdateSecurityGroup(sec string) error
}

type securityGroupManager struct {
	lock          sync.RWMutex
	securityGroup string
}

func (m *securityGroupManager) GetSecurityGroup() string {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return m.securityGroup
}

func (m *securityGroupManager) UpdateSecurityGroup(sec string) error {
	if len(sec) == 0 {
		return fmt.Errorf("security group is empty")
	}
	m.lock.Lock()
	defer m.lock.Unlock()
	m.securityGroup = sec
	log.Infof("SecurityGroup update to %v", m.securityGroup)
	return nil
}

func NewSecurityGroupManager() SecurityGroupManager {
	return &securityGroupManager{
		lock:          sync.RWMutex{},
		securityGroup: "",
	}
}
