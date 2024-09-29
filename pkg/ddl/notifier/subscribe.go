// Copyright 2024 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package notifier

import (
	"context"
	goerr "errors"
	"fmt"
	"time"

	"github.com/pingcap/errors"
	sess "github.com/pingcap/tidb/pkg/ddl/session"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"go.uber.org/zap"
)

// SchemaChangeHandler function is used by subscribers to handle the
// SchemaChangeEvent generated by the publisher (DDL module currently). It will
// be called at least once for every SchemaChange. The sctx has already started a
// pessimistic transaction and handler should execute exactly once SQL
// modification logic with it. After the function is returned, subscribing
// framework will commit the whole transaction with internal flag modification to
// provide exactly-once delivery. The handler will be called periodically, with
// no guarantee about the latency between the execution time and
// SchemaChangeEvent happening time.
//
// The handler function must be registered by RegisterHandler before the
// ddlNotifier is started. If the handler can't immediately serve the handling
// after registering, it can return nil to tell the ddlNotifier to act like the
// change has been handled, or return ErrNotReadyRetryLater to hold the change
// and re-handle later.
type SchemaChangeHandler func(
	ctx context.Context,
	sctx sessionctx.Context,
	change *SchemaChangeEvent,
) error

// ErrNotReadyRetryLater should be returned by a registered handler that is not
// ready to process the events.
var ErrNotReadyRetryLater = errors.New("not ready, retry later")

// HandlerID is the type of the persistent ID used to register a handler. Every
// ID occupies a bit in a BIGINT column, so at most we can only have 64 IDs. To
// avoid duplicate IDs, all IDs should be defined in below declaration.
type HandlerID int

const (
	// TestHandlerID is used for testing only.
	TestHandlerID HandlerID = 0
)

// String implements fmt.Stringer interface.
func (id HandlerID) String() string {
	switch id {
	case TestHandlerID:
		return "TestHandler"
	default:
		return fmt.Sprintf("HandlerID(%d)", id)
	}
}

// RegisterHandler must be called with an exclusive and fixed HandlerID for each
// handler to register the handler. Illegal ID will panic. RegisterHandler should
// not be called after the global ddlNotifier is started.
//
// RegisterHandler is not concurrency-safe.
func RegisterHandler(id HandlerID, handler SchemaChangeHandler) {
	intID := int(id)
	// the ID is used by bit operation in processedByFlag. We use BIGINT UNSIGNED to
	// store it so only 64 IDs are allowed.
	if intID < 0 || intID >= 64 {
		panic(fmt.Sprintf("illegal HandlerID: %d", id))
	}

	if _, ok := globalDDLNotifier.handlers[id]; ok {
		panic(fmt.Sprintf("HandlerID %d already registered", id))
	}
	globalDDLNotifier.handlers[id] = handler
}

type ddlNotifier struct {
	ownedSCtx    sessionctx.Context
	store        Store
	handlers     map[HandlerID]SchemaChangeHandler
	pollInterval time.Duration

	// handlersBitMap is set to the full bitmap of all registered handlers in Start.
	handlersBitMap uint64
}

// TODO(lance6716): remove this global variable. Move it into Domain and make
// related functions a member of it.
var globalDDLNotifier *ddlNotifier

// InitDDLNotifier initializes the global ddlNotifier. It should be called only
// once and before any RegisterHandler call. The ownership of the sctx is passed
// to the ddlNotifier.
func InitDDLNotifier(
	sctx sessionctx.Context,
	store Store,
	pollInterval time.Duration,
) {
	globalDDLNotifier = &ddlNotifier{
		ownedSCtx:    sctx,
		store:        store,
		handlers:     make(map[HandlerID]SchemaChangeHandler),
		pollInterval: pollInterval,
	}
}

// ResetDDLNotifier is used for testing only.
func ResetDDLNotifier() { globalDDLNotifier = nil }

// StartDDLNotifier starts the global ddlNotifier. It will block until the
// context is canceled.
func StartDDLNotifier(ctx context.Context) {
	globalDDLNotifier.Start(ctx)
}

// Start starts the ddlNotifier. It will block until the context is canceled.
func (n *ddlNotifier) Start(ctx context.Context) {
	for id := range n.handlers {
		n.handlersBitMap |= 1 << id
	}

	ctx = kv.WithInternalSourceType(ctx, kv.InternalDDLNotifier)
	ctx = logutil.WithCategory(ctx, "ddl-notifier")
	ticker := time.NewTicker(n.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := n.processEvents(ctx); err != nil {
				logutil.Logger(ctx).Error("Error processing events", zap.Error(err))
			}
		}
	}
}

func (n *ddlNotifier) processEvents(ctx context.Context) error {
	changes, err := n.store.List(ctx, sess.NewSession(n.ownedSCtx))
	if err != nil {
		return errors.Trace(err)
	}

	// we should ensure deliver order of events to a handler, so if a handler returns
	// error for previous events it should not receive later events.
	skipHandlers := make(map[HandlerID]struct{})
	for _, change := range changes {
		for handlerID, handler := range n.handlers {
			if _, ok := skipHandlers[handlerID]; ok {
				continue
			}
			if err2 := n.processEventForHandler(ctx, change, handlerID, handler); err2 != nil {
				skipHandlers[handlerID] = struct{}{}

				if !goerr.Is(err2, ErrNotReadyRetryLater) {
					logutil.Logger(ctx).Error("Error processing change",
						zap.Int64("ddlJobID", change.ddlJobID),
						zap.Int64("multiSchemaChangeSeq", change.multiSchemaChangeSeq),
						zap.Stringer("handler", handlerID),
						zap.Error(err2))
				}
				continue
			}
		}

		if change.processedByFlag == n.handlersBitMap {
			if err2 := n.store.DeleteAndCommit(
				ctx,
				sess.NewSession(n.ownedSCtx),
				change.ddlJobID,
				int(change.multiSchemaChangeSeq),
			); err2 != nil {
				logutil.Logger(ctx).Error("Error deleting change",
					zap.Int64("ddlJobID", change.ddlJobID),
					zap.Int64("multiSchemaChangeSeq", change.multiSchemaChangeSeq),
					zap.Error(err2))
			}
		}
	}

	return nil
}

const slowHandlerLogThreshold = time.Second * 5

func (n *ddlNotifier) processEventForHandler(
	ctx context.Context,
	change *schemaChange,
	handlerID HandlerID,
	handler SchemaChangeHandler,
) (err error) {
	if (change.processedByFlag & (1 << handlerID)) != 0 {
		return nil
	}

	se := sess.NewSession(n.ownedSCtx)

	if err = se.Begin(ctx); err != nil {
		return errors.Trace(err)
	}
	defer func() {
		if err == nil {
			err = errors.Trace(se.Commit(ctx))
		} else {
			se.Rollback()
		}
	}()

	now := time.Now()
	if err = handler(ctx, n.ownedSCtx, change.event); err != nil {
		return errors.Trace(err)
	}
	if time.Since(now) > slowHandlerLogThreshold {
		logutil.Logger(ctx).Warn("Slow process event",
			zap.Stringer("handler", handlerID),
			zap.Int64("ddlJobID", change.ddlJobID),
			zap.Int64("multiSchemaChangeSeq", change.multiSchemaChangeSeq),
			zap.Stringer("event", change.event),
			zap.Duration("duration", time.Since(now)))
	}

	newFlag := change.processedByFlag | (1 << handlerID)
	if err = n.store.UpdateProcessed(
		ctx,
		se,
		change.ddlJobID,
		change.multiSchemaChangeSeq,
		newFlag,
	); err != nil {
		return errors.Trace(err)
	}
	change.processedByFlag = newFlag

	return nil
}