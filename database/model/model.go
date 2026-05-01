// Package model defines the database models and data structures used by the 3x-ui panel.
package model

import (
	"fmt"

	"github.com/mhsanaei/3x-ui/v2/util/json_util"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

// Protocol represents the protocol type for Xray inbounds.
type Protocol string

// Protocol constants for different Xray inbound protocols
const (
	VMESS       Protocol = "vmess"
	VLESS       Protocol = "vless"
	Tunnel      Protocol = "tunnel"
	HTTP        Protocol = "http"
	Trojan      Protocol = "trojan"
	Shadowsocks Protocol = "shadowsocks"
	Mixed       Protocol = "mixed"
	WireGuard   Protocol = "wireguard"
	// UI stores Hysteria v1 and v2 both as "hysteria" and uses
	// settings.version to discriminate. Imports from outside the panel
	// can carry the literal "hysteria2" string, so IsHysteria below
	// accepts both.
	Hysteria  Protocol = "hysteria"
	Hysteria2 Protocol = "hysteria2"
)

// IsHysteria returns true for both "hysteria" and "hysteria2".
// Use instead of a bare ==model.Hysteria check: a v2 inbound stored
// with the literal v2 string would otherwise fall through (#4081).
func IsHysteria(p Protocol) bool {
	return p == Hysteria || p == Hysteria2
}

// User represents a user account in the 3x-ui panel.
type User struct {
	Id       int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// Inbound represents an Xray inbound configuration with traffic statistics and settings.
type Inbound struct {
	Id                   int                  `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`                                                    // Unique identifier
	UserId               int                  `json:"-"`                                                                                               // Associated user ID
	Up                   int64                `json:"up" form:"up"`                                                                                    // Upload traffic in bytes
	Down                 int64                `json:"down" form:"down"`                                                                                // Download traffic in bytes
	Total                int64                `json:"total" form:"total"`                                                                              // Total traffic limit in bytes
	AllTime              int64                `json:"allTime" form:"allTime" gorm:"default:0"`                                                         // All-time traffic usage
	Remark               string               `json:"remark" form:"remark"`                                                                            // Human-readable remark
	Enable               bool                 `json:"enable" form:"enable" gorm:"index:idx_enable_traffic_reset,priority:1"`                           // Whether the inbound is enabled
	ExpiryTime           int64                `json:"expiryTime" form:"expiryTime"`                                                                    // Expiration timestamp
	TrafficReset         string               `json:"trafficReset" form:"trafficReset" gorm:"default:never;index:idx_enable_traffic_reset,priority:2"` // Traffic reset schedule
	LastTrafficResetTime int64                `json:"lastTrafficResetTime" form:"lastTrafficResetTime" gorm:"default:0"`                               // Last traffic reset timestamp
	ClientStats          []xray.ClientTraffic `gorm:"foreignKey:InboundId;references:Id" json:"clientStats" form:"clientStats"`                        // Client traffic statistics

	// Xray configuration fields
	Listen         string   `json:"listen" form:"listen"`
	Port           int      `json:"port" form:"port"`
	Protocol       Protocol `json:"protocol" form:"protocol"`
	Settings       string   `json:"settings" form:"settings"`
	StreamSettings string   `json:"streamSettings" form:"streamSettings"`
	Tag            string   `json:"tag" form:"tag" gorm:"unique"`
	Sniffing       string   `json:"sniffing" form:"sniffing"`

	// Mesh: which node runs this inbound. NodeId=1 is the synthetic
	// "local" node — i.e. the panel host itself. Default is 1 so
	// upstream-3x-ui DBs migrate seamlessly: ALTER TABLE ADD COLUMN with
	// a default puts every existing inbound on the local node.
	NodeId int `json:"nodeId" form:"nodeId" gorm:"default:1;index"`
}

// OutboundTraffics tracks traffic statistics for Xray outbound connections.
type OutboundTraffics struct {
	Id    int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Tag   string `json:"tag" form:"tag" gorm:"unique"`
	Up    int64  `json:"up" form:"up" gorm:"default:0"`
	Down  int64  `json:"down" form:"down" gorm:"default:0"`
	Total int64  `json:"total" form:"total" gorm:"default:0"`
}

// InboundClientIps stores IP addresses associated with inbound clients for access control.
type InboundClientIps struct {
	Id          int    `json:"id" gorm:"primaryKey;autoIncrement"`
	ClientEmail string `json:"clientEmail" form:"clientEmail" gorm:"unique"`
	Ips         string `json:"ips" form:"ips"`
}

// HistoryOfSeeders tracks which database seeders have been executed to prevent re-running.
type HistoryOfSeeders struct {
	Id         int    `json:"id" gorm:"primaryKey;autoIncrement"`
	SeederName string `json:"seederName"`
}

// GenXrayInboundConfig generates an Xray inbound configuration from the Inbound model.
func (i *Inbound) GenXrayInboundConfig() *xray.InboundConfig {
	listen := i.Listen
	// Default to 0.0.0.0 (all interfaces) when listen is empty
	// This ensures proper dual-stack IPv4/IPv6 binding in systems where bindv6only=0
	if listen == "" {
		listen = "0.0.0.0"
	}
	listen = fmt.Sprintf("\"%v\"", listen)
	return &xray.InboundConfig{
		Listen:         json_util.RawMessage(listen),
		Port:           i.Port,
		Protocol:       string(i.Protocol),
		Settings:       json_util.RawMessage(i.Settings),
		StreamSettings: json_util.RawMessage(i.StreamSettings),
		Tag:            i.Tag,
		Sniffing:       json_util.RawMessage(i.Sniffing),
	}
}

// Setting stores key-value configuration settings for the 3x-ui panel.
type Setting struct {
	Id    int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Key   string `json:"key" form:"key"`
	Value string `json:"value" form:"value"`
}

type CustomGeoResource struct {
	Id            int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Type          string `json:"type" gorm:"not null;uniqueIndex:idx_custom_geo_type_alias;column:geo_type"`
	Alias         string `json:"alias" gorm:"not null;uniqueIndex:idx_custom_geo_type_alias"`
	Url           string `json:"url" gorm:"not null"`
	LocalPath     string `json:"localPath" gorm:"column:local_path"`
	LastUpdatedAt int64  `json:"lastUpdatedAt" gorm:"default:0;column:last_updated_at"`
	LastModified  string `json:"lastModified" gorm:"column:last_modified"`
	CreatedAt     int64  `json:"createdAt" gorm:"autoCreateTime;column:created_at"`
	UpdatedAt     int64  `json:"updatedAt" gorm:"autoUpdateTime;column:updated_at"`
}

// NodeStatus represents the connection state of a remote mesh node.
type NodeStatus string

const (
	// NodeStatusPending means the node row exists but no successful Pair has happened yet.
	NodeStatusPending NodeStatus = "pending"
	// NodeStatusConnected means the master has a healthy gRPC channel to this node.
	NodeStatusConnected NodeStatus = "connected"
	// NodeStatusDisconnected means the master previously paired with this node but cannot currently reach it.
	NodeStatusDisconnected NodeStatus = "disconnected"
	// NodeStatusError means the most recent ApplyConfig or Heartbeat failed in a non-network way.
	NodeStatusError NodeStatus = "error"
	// NodeStatusLocal is reserved for the synthetic id=1 row that always represents the panel host itself.
	NodeStatusLocal NodeStatus = "local"
)

// Node represents a host that runs xray-core under master control.
//
// id=1 is reserved: every install has a synthetic "local" Node row
// representing the panel host itself. Standalone-mode and master-mode
// panels both use this row for their own machine. Remote nodes paired
// from master mode get id>=2.
//
// Fields prefixed Api* describe the master->node control plane (gRPC).
// Address (no prefix) is what end-user clients connect to and what the
// subscription URL embeds — typically a public hostname. ApiAddress can
// be the same, or a separate private address when control traffic
// should ride a VPN/WireGuard tunnel.
type Node struct {
	Id        int        `json:"id" gorm:"primaryKey;autoIncrement"`
	Name      string     `json:"name" gorm:"unique;not null"`
	Address   string     `json:"address"` // public host that clients reach via subscription
	Port      int        `json:"port"`    // optional: override port for sub link generation; usually unused
	Status    NodeStatus `json:"status" gorm:"default:pending"`
	IsLocal   bool       `json:"isLocal" gorm:"default:false"`
	CreatedAt int64      `json:"createdAt" gorm:"autoCreateTime"`
	UpdatedAt int64      `json:"updatedAt" gorm:"autoUpdateTime"`

	// Control plane (only meaningful when IsLocal=false).
	ApiAddress string `json:"apiAddress"`
	ApiPort    int    `json:"apiPort"`

	// mTLS material. CaCertPem is the node's CA cert that the master
	// pinned on Pair; ClientCertPem/ClientKeyPem are the master's mTLS
	// client cert+key issued by that node CA. Stored on the master
	// side. v1: plaintext in SQLite; v2: encrypted with a panel-derived
	// key.
	CaCertPem     string `json:"-"`
	ClientCertPem string `json:"-"`
	ClientKeyPem  string `json:"-"`

	// Last-seen telemetry, updated by heartbeat.
	LastSeen     int64  `json:"lastSeen" gorm:"default:0"`
	Version      string `json:"version"`
	XrayVersion  string `json:"xrayVersion"`
	LastError    string `json:"lastError"`
	AppliedHash  string `json:"appliedHash"` // sha256 of last successful ApplyConfig payload
}

// NodeIdentity is a singleton (id=1 only) holding the keys/secrets a
// node needs to be paired with a master. Created on first switch into
// `node` mode; replaced on unpair. Lives only in node-mode panels;
// master/standalone panels have an empty table.
type NodeIdentity struct {
	Id              int    `gorm:"primaryKey;autoIncrement;check:id = 1"`
	NodeName        string `json:"nodeName"`
	CaCertPem       string `json:"-"` // this node's own CA, served to master during Pair
	CaKeyPem        string `json:"-"`
	ServerCertPem   string `json:"-"` // node's gRPC server cert (signed by own CA)
	ServerKeyPem    string `json:"-"`
	BootstrapToken  string `json:"-"` // one-shot pairing token, cleared after Pair
	BootstrapExpiry int64  `json:"bootstrapExpiry"`

	// Set after Pair succeeds.
	MasterClientFingerprint string `json:"masterClientFingerprint"` // sha256 of master's pinned client cert
	MasterName              string `json:"masterName"`
	MasterPairedAt          int64  `json:"masterPairedAt"`

	CreatedAt int64 `json:"createdAt" gorm:"autoCreateTime"`
	UpdatedAt int64 `json:"updatedAt" gorm:"autoUpdateTime"`
}

// Client represents a client configuration for Xray inbounds with traffic limits and settings.
type Client struct {
	ID         string `json:"id,omitempty"`                 // Unique client identifier
	Security   string `json:"security"`                     // Security method (e.g., "auto", "aes-128-gcm")
	Password   string `json:"password,omitempty"`           // Client password
	Flow       string `json:"flow,omitempty"`               // Flow control (XTLS)
	Auth       string `json:"auth,omitempty"`               // Auth password (Hysteria)
	Email      string `json:"email"`                        // Client email identifier
	LimitIP    int    `json:"limitIp"`                      // IP limit for this client
	TotalGB    int64  `json:"totalGB" form:"totalGB"`       // Total traffic limit in GB
	ExpiryTime int64  `json:"expiryTime" form:"expiryTime"` // Expiration timestamp
	Enable     bool   `json:"enable" form:"enable"`         // Whether the client is enabled
	TgID       int64  `json:"tgId" form:"tgId"`             // Telegram user ID for notifications
	SubID      string `json:"subId" form:"subId"`           // Subscription identifier
	Comment    string `json:"comment" form:"comment"`       // Client comment
	Reset      int    `json:"reset" form:"reset"`           // Reset period in days
	CreatedAt  int64  `json:"created_at,omitempty"`         // Creation timestamp
	UpdatedAt  int64  `json:"updated_at,omitempty"`         // Last update timestamp
}
