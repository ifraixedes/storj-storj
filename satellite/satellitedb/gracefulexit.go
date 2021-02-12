// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package satellitedb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/zeebo/errs"

	"storj.io/common/storj"
	"storj.io/storj/private/dbutil"
	"storj.io/storj/private/dbutil/pgutil"
	"storj.io/storj/private/tagsql"
	"storj.io/storj/satellite/gracefulexit"
	"storj.io/storj/satellite/metainfo/metabase"
	"storj.io/storj/satellite/satellitedb/dbx"
)

type gracefulexitDB struct {
	db *satelliteDB
}

// IncrementProgress increments transfer stats for a node.
func (db *gracefulexitDB) IncrementProgress(ctx context.Context, nodeID storj.NodeID, bytes int64, successfulTransfers int64, failedTransfers int64) (err error) {
	defer mon.Task()(&ctx)(&err)

	statement := db.db.Rebind(
		`INSERT INTO graceful_exit_progress (node_id, bytes_transferred, pieces_transferred, pieces_failed, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(node_id)
		 DO UPDATE SET bytes_transferred = graceful_exit_progress.bytes_transferred + excluded.bytes_transferred,
		 	pieces_transferred = graceful_exit_progress.pieces_transferred + excluded.pieces_transferred,
		 	pieces_failed = graceful_exit_progress.pieces_failed + excluded.pieces_failed,
		 	updated_at = excluded.updated_at;`,
	)
	now := time.Now().UTC()
	_, err = db.db.ExecContext(ctx, statement, nodeID, bytes, successfulTransfers, failedTransfers, now)
	if err != nil {
		return Error.Wrap(err)
	}

	return nil
}

// GetProgress gets a graceful exit progress entry.
func (db *gracefulexitDB) GetProgress(ctx context.Context, nodeID storj.NodeID) (_ *gracefulexit.Progress, err error) {
	defer mon.Task()(&ctx)(&err)
	dbxProgress, err := db.db.Get_GracefulExitProgress_By_NodeId(ctx, dbx.GracefulExitProgress_NodeId(nodeID.Bytes()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, gracefulexit.ErrNodeNotFound.Wrap(err)
	} else if err != nil {
		return nil, Error.Wrap(err)
	}
	nID, err := storj.NodeIDFromBytes(dbxProgress.NodeId)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	progress := &gracefulexit.Progress{
		NodeID:            nID,
		BytesTransferred:  dbxProgress.BytesTransferred,
		PiecesTransferred: dbxProgress.PiecesTransferred,
		PiecesFailed:      dbxProgress.PiecesFailed,
		UpdatedAt:         dbxProgress.UpdatedAt,
	}

	return progress, Error.Wrap(err)
}

// Enqueue batch inserts graceful exit transfer queue entries it does not exist.
func (db *gracefulexitDB) Enqueue(ctx context.Context, items []gracefulexit.TransferQueueItem) (err error) {
	defer mon.Task()(&ctx)(&err)

	sort.Slice(items, func(i, k int) bool {
		compare := bytes.Compare(items[i].NodeID.Bytes(), items[k].NodeID.Bytes())
		if compare == 0 {
			return bytes.Compare(items[i].Key, items[k].Key) < 0
		}
		return compare < 0
	})

	const crdbBatch = 1000

	b, e := 0, len(items)
	if db.db.implementation == dbutil.Cockroach {
		if len(items) > crdbBatch {
			e = crdbBatch
		}
	}

	for {
		var nodeIDs []storj.NodeID
		var keys [][]byte
		var pieceNums []int32
		var rootPieceIDs [][]byte
		var durabilities []float64
		for _, item := range items[b:e] {
			nodeIDs = append(nodeIDs, item.NodeID)
			keys = append(keys, item.Key)
			pieceNums = append(pieceNums, item.PieceNum)
			rootPieceIDs = append(rootPieceIDs, item.RootPieceID.Bytes())
			durabilities = append(durabilities, item.DurabilityRatio)
		}

		_, err = db.db.ExecContext(ctx, db.db.Rebind(`
			INSERT INTO graceful_exit_transfer_queue(node_id, path, piece_num, root_piece_id, durability_ratio, queued_at)
			SELECT unnest($1::bytea[]), unnest($2::bytea[]), unnest($3::int4[]), unnest($4::bytea[]), unnest($5::float8[]), $6
			ON CONFLICT DO NOTHING;`), pgutil.NodeIDArray(nodeIDs), pgutil.ByteaArray(keys), pgutil.Int4Array(pieceNums), pgutil.ByteaArray(rootPieceIDs), pgutil.Float8Array(durabilities), time.Now().UTC())

		if err != nil {
			return Error.Wrap(err)
		}

		if e < len(items) {
			if a := e + crdbBatch; a < len(items) {
				e = a
			} else {
				e = len(items)
			}
		} else {
			break
		}
	}

	return nil
}

// UpdateTransferQueueItem creates a graceful exit transfer queue entry.
func (db *gracefulexitDB) UpdateTransferQueueItem(ctx context.Context, item gracefulexit.TransferQueueItem) (err error) {
	defer mon.Task()(&ctx)(&err)
	update := dbx.GracefulExitTransferQueue_Update_Fields{
		DurabilityRatio: dbx.GracefulExitTransferQueue_DurabilityRatio(item.DurabilityRatio),
		LastFailedCode:  dbx.GracefulExitTransferQueue_LastFailedCode_Raw(item.LastFailedCode),
		FailedCount:     dbx.GracefulExitTransferQueue_FailedCount_Raw(item.FailedCount),
	}

	if item.RequestedAt != nil {
		update.RequestedAt = dbx.GracefulExitTransferQueue_RequestedAt_Raw(item.RequestedAt)
	}
	if item.LastFailedAt != nil {
		update.LastFailedAt = dbx.GracefulExitTransferQueue_LastFailedAt_Raw(item.LastFailedAt)
	}
	if item.FinishedAt != nil {
		update.FinishedAt = dbx.GracefulExitTransferQueue_FinishedAt_Raw(item.FinishedAt)
	}

	return db.db.UpdateNoReturn_GracefulExitTransferQueue_By_NodeId_And_Path_And_PieceNum(ctx,
		dbx.GracefulExitTransferQueue_NodeId(item.NodeID.Bytes()),
		dbx.GracefulExitTransferQueue_Path(item.Key),
		dbx.GracefulExitTransferQueue_PieceNum(int(item.PieceNum)),
		update,
	)
}

// DeleteTransferQueueItem deletes a graceful exit transfer queue entry.
func (db *gracefulexitDB) DeleteTransferQueueItem(ctx context.Context, nodeID storj.NodeID, key metabase.SegmentKey, pieceNum int32) (err error) {
	defer mon.Task()(&ctx)(&err)
	_, err = db.db.Delete_GracefulExitTransferQueue_By_NodeId_And_Path_And_PieceNum(ctx, dbx.GracefulExitTransferQueue_NodeId(nodeID.Bytes()), dbx.GracefulExitTransferQueue_Path(key),
		dbx.GracefulExitTransferQueue_PieceNum(int(pieceNum)))
	return Error.Wrap(err)
}

// DeleteTransferQueueItem deletes a graceful exit transfer queue entries by nodeID.
func (db *gracefulexitDB) DeleteTransferQueueItems(ctx context.Context, nodeID storj.NodeID) (err error) {
	defer mon.Task()(&ctx)(&err)
	_, err = db.db.Delete_GracefulExitTransferQueue_By_NodeId(ctx, dbx.GracefulExitTransferQueue_NodeId(nodeID.Bytes()))
	return Error.Wrap(err)
}

// DeleteFinishedTransferQueueItem deletes finished graceful exit transfer queue entries by nodeID.
func (db *gracefulexitDB) DeleteFinishedTransferQueueItems(ctx context.Context, nodeID storj.NodeID) (err error) {
	defer mon.Task()(&ctx)(&err)
	_, err = db.db.Delete_GracefulExitTransferQueue_By_NodeId_And_FinishedAt_IsNot_Null(ctx, dbx.GracefulExitTransferQueue_NodeId(nodeID.Bytes()))
	return Error.Wrap(err)
}

// DeleteAllFinishedTransferQueueItems deletes all graceful exit transfer
// queue items whose nodes have finished the exit before the indicated time
// returning the total number of deleted items.
func (db *gracefulexitDB) DeleteAllFinishedTransferQueueItems(
	ctx context.Context, before time.Time) (_ int64, err error) {
	defer mon.Task()(&ctx)(&err)

	switch db.db.implementation {
	case dbutil.Postgres:
		statement := `
			DELETE FROM graceful_exit_transfer_queue
			WHERE node_id IN (
				SELECT id
				FROM nodes
				WHERE exit_finished_at IS NOT NULL
					AND exit_finished_at < $1
			)`
		res, err := db.db.ExecContext(ctx, statement, before)
		if err != nil {
			return 0, Error.Wrap(err)
		}

		count, err := res.RowsAffected()
		if err != nil {
			return 0, Error.Wrap(err)
		}

		return count, nil

	case dbutil.Cockroach:
		const batch = 1000

		nodesQuery := fmt.Sprintf(`
			SELECT id
			FROM nodes %s
			WHERE exit_finished_at IS NOT NULL
				AND exit_finished_at < $1
			LIMIT $2 OFFSET $3
		`, db.db.AsOfSystemTimeClauseAt(time.Now().UTC()))
		deleteStm := `
			DELETE FROM graceful_exit_transfer_queue
			WHERE node_id IN (
				SELECT unnest($1::bytea[])
			)
			LIMIT $2
		`
		var (
			deleteCount int64
			offset      int
		)
		for {
			nodeIDs := make([][]byte, 0)

			// Select exited nodes
			rows, err := db.db.QueryContext(ctx, nodesQuery, before, batch, offset)
			if err != nil {
				return deleteCount, Error.Wrap(err)
			}

			count := 0
			for rows.Next() {
				var id storj.NodeID
				if err = rows.Scan(&id); err != nil {
					return deleteCount, Error.Wrap(err)
				}

				nodeIDs = append(nodeIDs, id.Bytes())
				count++
			}

			if count == batch {
				offset += count
			} else {
				offset = -1 // indicates that there aren't more nodes to query
			}

			// Delete, in batch, the GE query items of those nodes
			for {
				res, err := db.db.ExecContext(ctx, deleteStm, pgutil.ByteaArray(nodeIDs), batch)
				if err != nil {
					return deleteCount, Error.Wrap(err)
				}

				count, err := res.RowsAffected()
				if err != nil {
					return deleteCount, Error.Wrap(err)
				}

				deleteCount += count
				if count < batch {
					break
				}
			}

			// when offset is negative means that we have get already all the nodes
			// which have exited
			if offset < 0 {
				break
			}
		}

		return deleteCount, nil
	}

	return 0, Error.New("unsupported implementation: %s",
		dbutil.SchemeForImplementation(db.db.implementation),
	)
}

// GetTransferQueueItem gets a graceful exit transfer queue entry.
func (db *gracefulexitDB) GetTransferQueueItem(ctx context.Context, nodeID storj.NodeID, key metabase.SegmentKey, pieceNum int32) (_ *gracefulexit.TransferQueueItem, err error) {
	defer mon.Task()(&ctx)(&err)
	dbxTransferQueue, err := db.db.Get_GracefulExitTransferQueue_By_NodeId_And_Path_And_PieceNum(ctx,
		dbx.GracefulExitTransferQueue_NodeId(nodeID.Bytes()),
		dbx.GracefulExitTransferQueue_Path(key),
		dbx.GracefulExitTransferQueue_PieceNum(int(pieceNum)))
	if err != nil {
		return nil, Error.Wrap(err)
	}

	transferQueueItem, err := dbxToTransferQueueItem(dbxTransferQueue)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	return transferQueueItem, Error.Wrap(err)
}

// GetIncomplete gets incomplete graceful exit transfer queue entries ordered by durability ratio and queued date ascending.
func (db *gracefulexitDB) GetIncomplete(ctx context.Context, nodeID storj.NodeID, limit int, offset int64) (_ []*gracefulexit.TransferQueueItem, err error) {
	defer mon.Task()(&ctx)(&err)
	sql := `SELECT node_id, path, piece_num, root_piece_id, durability_ratio, queued_at, requested_at, last_failed_at, last_failed_code, failed_count, finished_at, order_limit_send_count
			FROM graceful_exit_transfer_queue
			WHERE node_id = ?
			AND finished_at is NULL
			ORDER BY durability_ratio asc, queued_at asc LIMIT ? OFFSET ?`
	rows, err := db.db.Query(ctx, db.db.Rebind(sql), nodeID.Bytes(), limit, offset)
	if err != nil {
		return nil, Error.Wrap(err)
	}
	defer func() { err = errs.Combine(err, rows.Close()) }()

	transferQueueItemRows, err := scanRows(rows)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	return transferQueueItemRows, nil
}

// GetIncompleteNotFailed gets incomplete graceful exit transfer queue entries that haven't failed, ordered by durability ratio and queued date ascending.
func (db *gracefulexitDB) GetIncompleteNotFailed(ctx context.Context, nodeID storj.NodeID, limit int, offset int64) (_ []*gracefulexit.TransferQueueItem, err error) {
	defer mon.Task()(&ctx)(&err)
	sql := `SELECT node_id, path, piece_num, root_piece_id, durability_ratio, queued_at, requested_at, last_failed_at, last_failed_code, failed_count, finished_at, order_limit_send_count
			FROM graceful_exit_transfer_queue
			WHERE node_id = ?
			AND finished_at is NULL
			AND last_failed_at is NULL
			ORDER BY durability_ratio asc, queued_at asc LIMIT ? OFFSET ?`
	rows, err := db.db.Query(ctx, db.db.Rebind(sql), nodeID.Bytes(), limit, offset)
	if err != nil {
		return nil, Error.Wrap(err)
	}
	defer func() { err = errs.Combine(err, rows.Close()) }()

	transferQueueItemRows, err := scanRows(rows)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	return transferQueueItemRows, nil
}

// GetIncompleteNotFailed gets incomplete graceful exit transfer queue entries that have failed <= maxFailures times, ordered by durability ratio and queued date ascending.
func (db *gracefulexitDB) GetIncompleteFailed(ctx context.Context, nodeID storj.NodeID, maxFailures int, limit int, offset int64) (_ []*gracefulexit.TransferQueueItem, err error) {
	defer mon.Task()(&ctx)(&err)
	sql := `SELECT node_id, path, piece_num, root_piece_id, durability_ratio, queued_at, requested_at, last_failed_at, last_failed_code, failed_count, finished_at, order_limit_send_count
			FROM graceful_exit_transfer_queue
			WHERE node_id = ?
			AND finished_at is NULL
			AND last_failed_at is not NULL
			AND failed_count < ?
			ORDER BY durability_ratio asc, queued_at asc LIMIT ? OFFSET ?`
	rows, err := db.db.Query(ctx, db.db.Rebind(sql), nodeID.Bytes(), maxFailures, limit, offset)
	if err != nil {
		return nil, Error.Wrap(err)
	}
	defer func() { err = errs.Combine(err, rows.Close()) }()

	transferQueueItemRows, err := scanRows(rows)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	return transferQueueItemRows, nil
}

// IncrementOrderLimitSendCount increments the number of times a node has been sent an order limit for transferring.
func (db *gracefulexitDB) IncrementOrderLimitSendCount(ctx context.Context, nodeID storj.NodeID, key metabase.SegmentKey, pieceNum int32) (err error) {
	defer mon.Task()(&ctx)(&err)

	sql := db.db.Rebind(
		`UPDATE graceful_exit_transfer_queue SET order_limit_send_count = graceful_exit_transfer_queue.order_limit_send_count + 1
		WHERE node_id = ?
		AND path = ?
		AND piece_num = ?`,
	)
	_, err = db.db.ExecContext(ctx, sql, nodeID, key, pieceNum)
	return Error.Wrap(err)
}

// CountFinishedTransferQueueItemsByNode return a map of the nodes which has
// finished the exit before the indicated time but there are at least one item
// left in the transfer queue.
func (db *gracefulexitDB) CountFinishedTransferQueueItemsByNode(ctx context.Context, before time.Time) (_ map[storj.NodeID]int64, err error) {
	defer mon.Task()(&ctx)(&err)

	asOf := ""
	if db.db.implementation == dbutil.Cockroach {
		asOf = db.db.AsOfSystemTimeClauseAt(time.Now().UTC())
	}

	query := fmt.Sprintf(
		`SELECT n.id, count(getq.node_id)
		FROM nodes as n LEFT JOIN graceful_exit_transfer_queue as getq
			ON n.id = getq.node_id %s
		WHERE n.exit_finished_at IS NOT NULL
			AND n.exit_finished_at < ?
		GROUP BY n.id
		HAVING count(getq.node_id) > 0`,
		asOf,
	)

	statement := db.db.Rebind(query)

	rows, err := db.db.QueryContext(ctx, statement, before)
	if err != nil {
		return nil, Error.Wrap(err)
	}
	defer func() { err = errs.Combine(err, Error.Wrap(rows.Close())) }()

	nodesItemsCount := make(map[storj.NodeID]int64)
	for rows.Next() {
		var (
			nodeID storj.NodeID
			n      int64
		)
		err := rows.Scan(&nodeID, &n)
		if err != nil {
			return nil, Error.Wrap(err)
		}

		nodesItemsCount[nodeID] = n
	}

	return nodesItemsCount, Error.Wrap(rows.Err())
}

func scanRows(rows tagsql.Rows) (transferQueueItemRows []*gracefulexit.TransferQueueItem, err error) {
	for rows.Next() {
		transferQueueItem := &gracefulexit.TransferQueueItem{}
		var pieceIDBytes []byte
		err = rows.Scan(&transferQueueItem.NodeID, &transferQueueItem.Key, &transferQueueItem.PieceNum, &pieceIDBytes,
			&transferQueueItem.DurabilityRatio, &transferQueueItem.QueuedAt, &transferQueueItem.RequestedAt, &transferQueueItem.LastFailedAt,
			&transferQueueItem.LastFailedCode, &transferQueueItem.FailedCount, &transferQueueItem.FinishedAt, &transferQueueItem.OrderLimitSendCount)
		if err != nil {
			return nil, Error.Wrap(err)
		}
		if pieceIDBytes != nil {
			transferQueueItem.RootPieceID, err = storj.PieceIDFromBytes(pieceIDBytes)
			if err != nil {
				return nil, Error.Wrap(err)
			}
		}

		transferQueueItemRows = append(transferQueueItemRows, transferQueueItem)
	}
	return transferQueueItemRows, Error.Wrap(rows.Err())
}

func dbxToTransferQueueItem(dbxTransferQueue *dbx.GracefulExitTransferQueue) (item *gracefulexit.TransferQueueItem, err error) {
	nID, err := storj.NodeIDFromBytes(dbxTransferQueue.NodeId)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	item = &gracefulexit.TransferQueueItem{
		NodeID:              nID,
		Key:                 dbxTransferQueue.Path,
		PieceNum:            int32(dbxTransferQueue.PieceNum),
		DurabilityRatio:     dbxTransferQueue.DurabilityRatio,
		QueuedAt:            dbxTransferQueue.QueuedAt,
		OrderLimitSendCount: dbxTransferQueue.OrderLimitSendCount,
	}
	if dbxTransferQueue.RootPieceId != nil {
		item.RootPieceID, err = storj.PieceIDFromBytes(dbxTransferQueue.RootPieceId)
		if err != nil {
			return nil, err
		}
	}
	if dbxTransferQueue.LastFailedCode != nil {
		item.LastFailedCode = dbxTransferQueue.LastFailedCode
	}
	if dbxTransferQueue.FailedCount != nil {
		item.FailedCount = dbxTransferQueue.FailedCount
	}
	if dbxTransferQueue.RequestedAt != nil && !dbxTransferQueue.RequestedAt.IsZero() {
		item.RequestedAt = dbxTransferQueue.RequestedAt
	}
	if dbxTransferQueue.LastFailedAt != nil && !dbxTransferQueue.LastFailedAt.IsZero() {
		item.LastFailedAt = dbxTransferQueue.LastFailedAt
	}
	if dbxTransferQueue.FinishedAt != nil && !dbxTransferQueue.FinishedAt.IsZero() {
		item.FinishedAt = dbxTransferQueue.FinishedAt
	}

	return item, nil
}
