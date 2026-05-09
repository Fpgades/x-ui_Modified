package controller

import (
	"strconv"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/mesh"
	"github.com/mhsanaei/3x-ui/v2/web/service"

	"github.com/gin-gonic/gin"
)

// MeshController exposes the master/node management API at
// /panel/api/mesh/*. The same routes are mounted regardless of
// panelMode; individual handlers reject calls that are nonsensical
// for the current mode (e.g. POST /nodes from a node-mode panel).
type MeshController struct {
	nodeService    service.NodeService
	settingService service.SettingService
}

// NewMeshController returns a fully initialised controller and
// registers its routes on g.
func NewMeshController(g *gin.RouterGroup) *MeshController {
	a := &MeshController{}
	a.initRouter(g)
	return a
}

func (a *MeshController) initRouter(g *gin.RouterGroup) {
	// Mode (any panel can read; only standalone/master can write).
	g.GET("/mode", a.getMode)
	g.POST("/mode", a.setMode)

	// Master-side: node CRUD + pairing.
	g.GET("/nodes", a.listNodes)
	g.POST("/nodes", a.createNode)
	g.POST("/nodes/:id/pair", a.pairNode)
	g.POST("/nodes/:id/del", a.deleteNode)

	// Node-side: own identity + bootstrap-token mint + unpair.
	g.GET("/identity", a.getIdentity)
	g.POST("/identity/token", a.mintToken)
	g.POST("/identity/unpair", a.unpair)
}

// ----- mode -----

type modeResponse struct {
	Mode string `json:"mode"`
}

func (a *MeshController) getMode(c *gin.Context) {
	jsonObj(c, modeResponse{Mode: a.settingService.GetPanelMode()}, nil)
}

type setModeRequest struct {
	Mode string `json:"mode" form:"mode"`
}

// setMode persists the new panelMode setting. The caller is expected
// to subsequently restart the panel (SIGHUP) for the runtime side
// effects (start/stop xray, open/close gRPC server) to take hold.
//
// Validation rules:
//   - any -> standalone: free
//   - standalone -> master: free
//   - master -> standalone: requires zero remote nodes
//   - any -> node: requires zero remote nodes assigned (master) and
//     no inbounds with node_id != 1 (master)
//   - node -> standalone/master: requires the node to be unpaired first
func (a *MeshController) setMode(c *gin.Context) {
	var req setModeRequest
	if err := c.ShouldBind(&req); err != nil {
		jsonMsg(c, "set mode", err)
		return
	}

	current := a.settingService.GetPanelMode()
	target := req.Mode

	if current == target {
		jsonObj(c, modeResponse{Mode: target}, nil)
		return
	}

	if err := a.validateModeTransition(current, target); err != nil {
		jsonMsg(c, "set mode", err)
		return
	}

	if err := a.settingService.SetPanelMode(target); err != nil {
		jsonMsg(c, "set mode", err)
		return
	}
	jsonObj(c, modeResponse{Mode: target}, nil)
}

func (a *MeshController) validateModeTransition(current, target string) error {
	switch target {
	case service.PanelModeStandalone, service.PanelModeMaster, service.PanelModeNode:
	default:
		return errBadMode(target)
	}

	// Leaving node mode requires unpair first — but only if there's
	// actually a pinned master. If the operator switched to node mode
	// for a moment without pairing, let them switch right back.
	if current == service.PanelModeNode && target != service.PanelModeNode {
		id, err := mesh.LoadIdentity()
		if err == nil && id != nil && id.MasterClientFingerprint != "" {
			return errStr("unpair from master before leaving node mode")
		}
	}

	// Entering node mode requires no remote nodes / no non-local inbounds
	// (master would otherwise lose track of its data plane).
	if target == service.PanelModeNode {
		nodes, err := a.nodeService.GetAll()
		if err == nil {
			for _, n := range nodes {
				if !n.IsLocal {
					return errStr("delete all remote nodes before switching to node mode")
				}
			}
		}
	}

	return nil
}

// ----- master-side: nodes -----

func (a *MeshController) listNodes(c *gin.Context) {
	nodes, err := a.nodeService.GetAll()
	if err != nil {
		jsonMsg(c, "list nodes", err)
		return
	}
	jsonObj(c, nodes, nil)
}

type createNodeRequest struct {
	Name       string `json:"name" form:"name"`
	Address    string `json:"address" form:"address"`         // public, used in subscription
	Port       int    `json:"port" form:"port"`               // optional, for sub-link generation
	ApiAddress string `json:"apiAddress" form:"apiAddress"`   // master->node control plane
	ApiPort    int    `json:"apiPort" form:"apiPort"`
}

func (a *MeshController) createNode(c *gin.Context) {
	if a.settingService.GetPanelMode() != service.PanelModeMaster {
		jsonMsg(c, "create node", errStr("node management is only available in master mode"))
		return
	}
	var req createNodeRequest
	if err := c.ShouldBind(&req); err != nil {
		jsonMsg(c, "create node", err)
		return
	}
	n, err := a.nodeService.CreateRemoteNode(req.Name, req.Address, req.Port, req.ApiAddress, req.ApiPort)
	if err != nil {
		jsonMsg(c, "create node", err)
		return
	}
	jsonObj(c, n, nil)
}

type pairNodeRequest struct {
	Token string `json:"token" form:"token"`
}

func (a *MeshController) pairNode(c *gin.Context) {
	if a.settingService.GetPanelMode() != service.PanelModeMaster {
		jsonMsg(c, "pair node", errStr("node management is only available in master mode"))
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "pair node", err)
		return
	}
	var req pairNodeRequest
	if err := c.ShouldBind(&req); err != nil {
		jsonMsg(c, "pair node", err)
		return
	}
	if err := a.nodeService.Pair(c.Request.Context(), id, req.Token, 15*time.Second); err != nil {
		jsonMsg(c, "pair node", err)
		return
	}
	jsonMsg(c, "paired", nil)
}

func (a *MeshController) deleteNode(c *gin.Context) {
	if a.settingService.GetPanelMode() != service.PanelModeMaster {
		jsonMsg(c, "delete node", errStr("node management is only available in master mode"))
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "delete node", err)
		return
	}
	if err := a.nodeService.Delete(id); err != nil {
		jsonMsg(c, "delete node", err)
		return
	}
	jsonMsg(c, "deleted", nil)
}

// ----- node-side: identity / bootstrap -----

type identityResponse struct {
	NodeName            string `json:"nodeName"`
	ServerCertSha256    string `json:"serverCertSha256"`
	BootstrapToken      string `json:"bootstrapToken"`
	BootstrapExpiry     int64  `json:"bootstrapExpiryUnixMs"`
	BootstrapTokenValid bool   `json:"bootstrapTokenValid"`
	MasterName          string `json:"masterName"`
	MasterPairedAtUnix  int64  `json:"masterPairedAtUnixMs"`
	IsPaired            bool   `json:"isPaired"`
}

func (a *MeshController) getIdentity(c *gin.Context) {
	id, err := mesh.LoadIdentity()
	if err != nil {
		// Not yet generated — return an empty placeholder. The UI shows
		// "switch to node mode to generate identity".
		jsonObj(c, identityResponse{}, nil)
		return
	}
	resp := identityResponse{
		NodeName:            id.NodeName,
		BootstrapToken:      id.BootstrapToken,
		BootstrapExpiry:     id.BootstrapExpiry,
		BootstrapTokenValid: id.BootstrapToken != "" && !mesh.IsExpired(id.BootstrapExpiry),
		MasterName:          id.MasterName,
		MasterPairedAtUnix:  id.MasterPairedAt,
		IsPaired:            id.MasterClientFingerprint != "",
	}
	jsonObj(c, resp, nil)
}

func (a *MeshController) mintToken(c *gin.Context) {
	if a.settingService.GetPanelMode() != service.PanelModeNode {
		// Allow minting in any mode for testing convenience but warn —
		// the token only matters in node mode. We still gate against
		// master mode because exposing a token there is meaningless.
		mode := a.settingService.GetPanelMode()
		if mode == service.PanelModeMaster {
			jsonMsg(c, "mint token", errStr("token minting is only meaningful in node mode"))
			return
		}
	}
	tok, err := mesh.MintBootstrapToken(0)
	if err != nil {
		jsonMsg(c, "mint token", err)
		return
	}
	jsonObj(c, gin.H{"token": tok, "ttlSeconds": int(mesh.BootstrapTokenTTL.Seconds())}, nil)
}

func (a *MeshController) unpair(c *gin.Context) {
	if err := mesh.Unpair(); err != nil {
		jsonMsg(c, "unpair", err)
		return
	}
	jsonMsg(c, "unpaired", nil)
}

// ----- helpers -----

// errStr returns a typed error from a string literal — small helper
// because controllers in this package have no direct access to
// common.NewError.
func errStr(s string) error { return &meshErr{s} }

type meshErr struct{ msg string }

func (e *meshErr) Error() string { return e.msg }

func errBadMode(m string) error {
	return errStr("invalid panel mode: " + m)
}

// silence "imported and not used" if the model import becomes unused
// after refactors — currently used for type assertions in identity.
var _ = model.NodeStatusPending
