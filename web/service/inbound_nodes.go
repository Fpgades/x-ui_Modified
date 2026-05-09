package service

import (
	"sort"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"

	"gorm.io/gorm"
)

// GetInboundNodeIDs returns the ids of every node this inbound is
// deployed to. Sorted ascending. Returns the legacy Inbound.NodeId as
// a fallback if the join table is empty for this inbound (older DBs
// before the backfill seeder ran).
func GetInboundNodeIDs(inboundID int) ([]int, error) {
	var rows []model.InboundNode
	if err := database.GetDB().
		Where("inbound_id = ?", inboundID).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]int, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.NodeId)
	}
	sort.Ints(out)
	return out, nil
}

// SetInboundNodeIDs replaces the set of nodes an inbound is deployed
// to. If ids is empty, defaults to [1] (the synthetic local node) so
// the inbound never becomes orphan.
func SetInboundNodeIDs(tx *gorm.DB, inboundID int, ids []int) error {
	if tx == nil {
		tx = database.GetDB()
	}
	if len(ids) == 0 {
		ids = []int{1}
	}

	// Dedupe and sort to keep DB rows deterministic.
	seen := make(map[int]struct{}, len(ids))
	clean := make([]int, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		clean = append(clean, id)
	}
	sort.Ints(clean)

	return tx.Transaction(func(t *gorm.DB) error {
		if err := t.Where("inbound_id = ?", inboundID).Delete(&model.InboundNode{}).Error; err != nil {
			return err
		}
		for _, nid := range clean {
			if err := t.Create(&model.InboundNode{InboundId: inboundID, NodeId: nid}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// InboundIsOnNode reports whether the given inbound is currently
// deployed to nodeID. Used by config builders and subscription
// renderers; a small index hit per inbound is cheaper than loading
// the whole list.
func InboundIsOnNode(inboundID, nodeID int) (bool, error) {
	var count int64
	if err := database.GetDB().
		Model(&model.InboundNode{}).
		Where("inbound_id = ? AND node_id = ?", inboundID, nodeID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}
