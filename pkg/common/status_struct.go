package common

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type LocalNodeStatuses []LocalNodeStatus

type LocalNodeStatus struct {
	Name    string `json:"name"`
	DomainName string `json:"domainName"`
	Status  string `json:"status"`
	LastHeartbeatTime metav1.Time `json:"lastHeartbeatTime,omitempty"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
	UnreadyReason string `json:"unreadyReason,omitempty"`
	TotalCPURequest int64 `json:"totalCPURequest"`
	TotalMemRequest int64 `json:"totalMemRequest"`
}
