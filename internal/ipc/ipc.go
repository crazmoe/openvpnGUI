// Package ipc definiert das gemeinsame Protokoll zwischen der GUI (cmd/gui)
// und dem privilegierten Daemon (cmd/daemon), die über einen Unix-Socket
// kommunizieren.
package ipc

import "encoding/json"

const SocketPath = "/run/vpn-manager.sock"

type Request struct {
	Action   string `json:"action"`
	Config   string `json:"config"`
	Username string `json:"username"`
	Password string `json:"password"`
	Content  string `json:"content"`
}

type Response struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type VPNStatus struct {
	State   string `json:"state"`
	LocalIP string `json:"local_ip"`
}
