// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package satellitedb_test

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"storj.io/common/pb"
	"storj.io/common/storj"
	"storj.io/common/testcontext"
	"storj.io/storj/private/testplanet"
	"storj.io/storj/satellite"
	"storj.io/storj/satellite/gracefulexit"
	"storj.io/storj/satellite/metainfo/metabase"
	"storj.io/storj/satellite/overlay"
	"storj.io/storj/satellite/satellitedb/satellitedbtest"
)

func TestGracefulExit_DeleteAllFinishedTransferQueueItems(t *testing.T) {
	testplanet.Run(t, testplanet.Config{
		SatelliteCount: 1, StorageNodeCount: 7,
	}, func(t *testing.T, ctx *testcontext.Context, planet *testplanet.Planet) {
		var (
			cache       = planet.Satellites[0].DB.OverlayCache()
			currentTime = time.Now()
		)

		// Mark some of the storagenodes as successful exit
		nodeSuccessful1 := planet.StorageNodes[1]
		_, err := cache.UpdateExitStatus(ctx, &overlay.ExitStatusRequest{
			NodeID:              nodeSuccessful1.ID(),
			ExitInitiatedAt:     currentTime.Add(-time.Hour),
			ExitLoopCompletedAt: currentTime.Add(-30 * time.Minute),
			ExitFinishedAt:      currentTime.Add(-25 * time.Minute),
			ExitSuccess:         true,
		})
		require.NoError(t, err)

		nodeSuccessful2 := planet.StorageNodes[2]
		_, err = cache.UpdateExitStatus(ctx, &overlay.ExitStatusRequest{
			NodeID:              nodeSuccessful2.ID(),
			ExitInitiatedAt:     currentTime.Add(-time.Hour),
			ExitLoopCompletedAt: currentTime.Add(-17 * time.Minute),
			ExitFinishedAt:      currentTime.Add(-16 * time.Minute),
			ExitSuccess:         true,
		})
		require.NoError(t, err)

		nodeSuccessful3 := planet.StorageNodes[3]
		_, err = cache.UpdateExitStatus(ctx, &overlay.ExitStatusRequest{
			NodeID:              nodeSuccessful3.ID(),
			ExitInitiatedAt:     currentTime.Add(-time.Hour),
			ExitLoopCompletedAt: currentTime.Add(-9 * time.Minute),
			ExitFinishedAt:      currentTime.Add(-5 * time.Minute),
			ExitSuccess:         true,
		})
		require.NoError(t, err)

		// Mark some of the storagenodes as failed exit
		nodeFailed1 := planet.StorageNodes[4]
		_, err = cache.UpdateExitStatus(ctx, &overlay.ExitStatusRequest{
			NodeID:              nodeFailed1.ID(),
			ExitInitiatedAt:     currentTime.Add(-time.Hour),
			ExitLoopCompletedAt: currentTime.Add(-28 * time.Minute),
			ExitFinishedAt:      currentTime.Add(-20 * time.Minute),
			ExitSuccess:         true,
		})
		require.NoError(t, err)

		nodeFailed2 := planet.StorageNodes[5]
		_, err = cache.UpdateExitStatus(ctx, &overlay.ExitStatusRequest{
			NodeID:              nodeFailed2.ID(),
			ExitInitiatedAt:     currentTime.Add(-time.Hour),
			ExitLoopCompletedAt: currentTime.Add(-17 * time.Minute),
			ExitFinishedAt:      currentTime.Add(-15 * time.Minute),
			ExitSuccess:         true,
		})
		require.NoError(t, err)

		nodeWithoutItems := planet.StorageNodes[6]
		_, err = cache.UpdateExitStatus(ctx, &overlay.ExitStatusRequest{
			NodeID:              nodeWithoutItems.ID(),
			ExitInitiatedAt:     currentTime.Add(-time.Hour),
			ExitLoopCompletedAt: currentTime.Add(-35 * time.Minute),
			ExitFinishedAt:      currentTime.Add(-32 * time.Minute),
			ExitSuccess:         false,
		})
		require.NoError(t, err)

		// Add some items to the transfer queue for the exited nodes
		queueItems, nodesItems := generateTransferQueueItems(t, []*testplanet.StorageNode{
			nodeSuccessful1, nodeSuccessful2, nodeSuccessful3, nodeFailed1, nodeFailed2,
		})

		gracefulExitDB := planet.Satellites[0].DB.GracefulExit()
		err = gracefulExitDB.Enqueue(ctx, queueItems)
		require.NoError(t, err)

		// Wait a bit so the next CRDB SQL queries, which use
		// 'AS OF SYSTEM TIME <<time.Now()>>' can find the above inserts.
		time.Sleep(time.Second)

		// Count nodes exited before 15 minutes ago
		nodes, err := gracefulExitDB.CountFinishedTransferQueueItemsByNode(ctx, currentTime.Add(-15*time.Minute))
		require.NoError(t, err)
		require.Len(t, nodes, 3, "invalid number of nodes which have exited 15 minutes ago")

		for id, n := range nodes {
			assert.EqualValues(t, nodesItems[id], n, "unexpected number of items")
		}

		// Count nodes exited before 4 minutes ago
		nodes, err = gracefulExitDB.CountFinishedTransferQueueItemsByNode(ctx, currentTime.Add(-4*time.Minute))
		require.NoError(t, err)
		require.Len(t, nodes, 5, "invalid number of nodes which have exited 4 minutes ago")

		for id, n := range nodes {
			assert.EqualValues(t, nodesItems[id], n, "unexpected number of items")
		}

		// Delete items of nodes exited before 15 minutes ago
		count, err := gracefulExitDB.DeleteAllFinishedTransferQueueItems(ctx, currentTime.Add(-15*time.Minute))
		require.NoError(t, err)
		expectedNumDeletedItems := nodesItems[nodeSuccessful1.ID()] +
			nodesItems[nodeSuccessful2.ID()] +
			nodesItems[nodeFailed1.ID()]
		require.EqualValues(t, expectedNumDeletedItems, count, "invalid number of deleted items")

		// Wait a bit so the next CRDB SQL queries, which use
		// 'AS OF SYSTEM TIME <<time.Now()>>' can find the above deletes
		time.Sleep(time.Second)

		// Check that only a few nodes have exited are left with items
		nodes, err = gracefulExitDB.CountFinishedTransferQueueItemsByNode(ctx, currentTime.Add(time.Minute))
		require.NoError(t, err)
		require.Len(t, nodes, 2, "invalid number of exited nodes with items")

		for id, n := range nodes {
			assert.EqualValues(t, nodesItems[id], n, "unexpected number of items")
			assert.NotEqual(t, nodeSuccessful1.ID(), id, "node shouldn't have items")
			assert.NotEqual(t, nodeSuccessful2.ID(), id, "node shouldn't have items")
			assert.NotEqual(t, nodeFailed1.ID(), id, "node shouldn't have items")
		}

		// Delete items of there rest exited nodes
		count, err = gracefulExitDB.DeleteAllFinishedTransferQueueItems(ctx, currentTime.Add(time.Minute))
		require.NoError(t, err)
		expectedNumDeletedItems = nodesItems[nodeSuccessful3.ID()] + nodesItems[nodeFailed2.ID()]
		require.EqualValues(t, expectedNumDeletedItems, count, "invalid number of deleted items")

		// Wait a bit so the next CRDB SQL queries, which use
		// 'AS OF SYSTEM TIME <<time.Now()>>' can find the above deletes
		time.Sleep(time.Second)

		// Check that there aren't more exited nodes with items
		nodes, err = gracefulExitDB.CountFinishedTransferQueueItemsByNode(ctx, currentTime.Add(time.Minute))
		require.NoError(t, err)
		require.Len(t, nodes, 0, "invalid number of exited nodes with items")
	})
}

// TestGracefulExit_DeleteAllFinishedTransferQueueItems_bigAmount verifies that
// a big amount of exited nodes with a decent amount of transfer queue items are
// counted and deleted properly.
//
// This test was mainly created for verifying the
// DeleteAllFinishedTransferQueueItems for CRDB because the query differs from
// Postgres and it has some logic to execute it in batches.
func TestGracefulExit_DeleteAllFinishedTransferQueueItems_bigAmount(t *testing.T) {
	rand.Seed(time.Now().UnixNano())

	satellitedbtest.Run(t, func(ctx *testcontext.Context, t *testing.T, db satellite.DB) {
		const (
			addr    = "127.0.1.0:8080"
			lastNet = "127.0.0"
		)
		var (
			numNonExitedNodes = rand.Intn(100) + 1
			numExitedNodes    = rand.Intn(1000) + 1001
			cache             = db.OverlayCache()
		)

		for i := 0; i < numNonExitedNodes; i++ {
			info := overlay.NodeCheckInInfo{
				NodeID:     generateNodeIDFromPostiveInt(t, i),
				Address:    &pb.NodeAddress{Address: addr, Transport: pb.NodeTransport_TCP_TLS_GRPC},
				LastIPPort: addr,
				LastNet:    lastNet,
				Version:    &pb.NodeVersion{Version: "v1.0.0"},
				Capacity:   &pb.NodeCapacity{},
				IsUp:       true,
			}
			err := cache.UpdateCheckIn(ctx, info, time.Now(), overlay.NodeSelectionConfig{})
			require.NoError(t, err)
		}

		var (
			currentTime   = time.Now()
			exitedNodeIDs = make([]storj.NodeID, 0, numNonExitedNodes)
			nodeIDsMap    = make(map[storj.NodeID]struct{})
		)
		for i := numNonExitedNodes; i < (numNonExitedNodes + numExitedNodes); i++ {
			nodeID := generateNodeIDFromPostiveInt(t, i)
			exitedNodeIDs = append(exitedNodeIDs, nodeID)
			if _, ok := nodeIDsMap[nodeID]; ok {
				fmt.Printf("this %v already exists\n", nodeID.Bytes())
			}
			nodeIDsMap[nodeID] = struct{}{}

			info := overlay.NodeCheckInInfo{
				NodeID:     nodeID,
				Address:    &pb.NodeAddress{Address: addr, Transport: pb.NodeTransport_TCP_TLS_GRPC},
				LastIPPort: addr,
				LastNet:    lastNet,
				Version:    &pb.NodeVersion{Version: "v1.0.0"},
				Capacity:   &pb.NodeCapacity{},
				IsUp:       true,
			}
			err := cache.UpdateCheckIn(ctx, info, time.Now(), overlay.NodeSelectionConfig{})
			require.NoError(t, err)

			exitFinishedAt := currentTime.Add(time.Duration(-(rand.Int63n(15) + 1)) * time.Minute)
			_, err = cache.UpdateExitStatus(ctx, &overlay.ExitStatusRequest{
				NodeID:              nodeID,
				ExitInitiatedAt:     exitFinishedAt.Add(-30 * time.Minute),
				ExitLoopCompletedAt: exitFinishedAt.Add(-20 * time.Minute),
				ExitFinishedAt:      exitFinishedAt,
				ExitSuccess:         true,
			})
			require.NoError(t, err)
		}

		require.Equal(t, numExitedNodes, len(nodeIDsMap), "map")

		// Add some items to the transfer queue for the exited nodes.
		// NOTE this numbers should be greater but CRDB test timeout if they are
		// greater while Postgres manage them quite well.
		queueItems := generateTransferQueueItemsMinMaxPerNode(t, 1, 25, exitedNodeIDs...)
		gracefulExitDB := db.GracefulExit()
		err := gracefulExitDB.Enqueue(ctx, queueItems)
		require.NoError(t, err)

		// Wait a bit so the next CRDB SQL queries, which use
		// 'AS OF SYSTEM TIME <<time.Now()>>' can find the above inserts.
		time.Sleep(time.Second)

		// Count exited nodes
		nodes, err := gracefulExitDB.CountFinishedTransferQueueItemsByNode(ctx, currentTime)
		require.NoError(t, err)
		require.EqualValues(t, numExitedNodes, len(nodes), "invalid number of exited nodes")

		// Delete items of the exited nodes
		count, err := gracefulExitDB.DeleteAllFinishedTransferQueueItems(ctx, currentTime)
		require.NoError(t, err)
		require.EqualValues(t, len(queueItems), count, "invalid number of deleted items")

		// Wait a bit so the next CRDB SQL queries, which use
		// 'AS OF SYSTEM TIME <<time.Now()>>' can find the above inserts.
		time.Sleep(time.Second)

		// Count exited nodes. At this time there shouldn't be any exited node with
		// items in the queue
		nodes, err = gracefulExitDB.CountFinishedTransferQueueItemsByNode(ctx, currentTime)
		require.NoError(t, err)
		require.Len(t, nodes, 0, "invalid number of exited nodes")

		// Delete items of the exited nodes. At this time there shouldn't be any
		count, err = gracefulExitDB.DeleteAllFinishedTransferQueueItems(ctx, currentTime.Add(-15*time.Minute))
		require.NoError(t, err)
		require.Zero(t, count, "invalid number of deleted items")
	})
}

// generateTransferQueueItems generates a random number of transfer queue items,
// between 10 and 120, for each passed node.
func generateTransferQueueItems(t *testing.T, nodes []*testplanet.StorageNode) ([]gracefulexit.TransferQueueItem, map[storj.NodeID]int64) {
	nodeIDs := make([]storj.NodeID, len(nodes))
	for i, n := range nodes {
		nodeIDs[i] = n.ID()
	}

	items := generateTransferQueueItemsMinMaxPerNode(t, 10, 120, nodeIDs...)

	nodesItems := make(map[storj.NodeID]int64, len(nodes))
	for _, item := range items {
		nodesItems[item.NodeID]++
	}

	return items, nodesItems
}

// generateTransferQueueItemsMinMaxPerNode generates a random number of transfer
// queue items, between minPerNode and maxPerNode, for each nodeIDs.
func generateTransferQueueItemsMinMaxPerNode(t *testing.T, minPerNode int, maxPerNode int, nodeIDs ...storj.NodeID) []gracefulexit.TransferQueueItem {
	if minPerNode < 0 && minPerNode >= maxPerNode || maxPerNode == 0 {
		t.Fatal("minPerNode cannot be negative, it must be less than maxPerNode, and maxPerNode cannot be 0")
	}

	items := make([]gracefulexit.TransferQueueItem, 0)
	for _, nodeID := range nodeIDs {
		n := rand.Intn(1+maxPerNode-minPerNode) + minPerNode
		for i := 0; i < n; i++ {
			items = append(items, gracefulexit.TransferQueueItem{
				NodeID:   nodeID,
				Key:      metabase.SegmentKey{byte(rand.Int31n(256))},
				PieceNum: rand.Int31(),
			})
		}
	}

	return items
}

// generateNodeIDFromPostiveInt generates a specific node ID for val; each val
// value produces a different node ID.
func generateNodeIDFromPostiveInt(t *testing.T, val int) storj.NodeID {
	t.Helper()

	if val < 0 {
		t.Fatal("cannot generate a node from a negative integer")
	}

	nodeID := storj.NodeID{}
	idx := 0
	for {
		m := val & 255
		nodeID[idx] = byte(m)

		q := val >> 8
		if q == 0 {
			break
		}
		if q < 256 {
			nodeID[idx+1] = byte(q)
			break
		}

		val = q
		idx++
	}

	return nodeID
}
