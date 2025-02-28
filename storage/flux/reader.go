package storageflux

import (
	"context"
	"fmt"
	"strings"

	"github.com/gogo/protobuf/types"
	"github.com/influxdata/flux"
	"github.com/influxdata/flux/execute"
	"github.com/influxdata/flux/memory"
	"github.com/influxdata/flux/values"
	"github.com/influxdata/influxdb/v2/kit/errors"
	"github.com/influxdata/influxdb/v2/models"
	"github.com/influxdata/influxdb/v2/query"
	storage "github.com/influxdata/influxdb/v2/storage/reads"
	"github.com/influxdata/influxdb/v2/storage/reads/datatypes"
	"github.com/influxdata/influxdb/v2/tsdb/cursors"
)

// GroupCursorError is returned when two different cursor types
// are read for the same table.
type GroupCursorError struct {
	typ    string
	cursor cursors.Cursor
}

func (err *GroupCursorError) Error() string {
	var got string
	switch err.cursor.(type) {
	case cursors.FloatArrayCursor:
		got = "float"
	case cursors.IntegerArrayCursor:
		got = "integer"
	case cursors.UnsignedArrayCursor:
		got = "unsigned"
	case cursors.StringArrayCursor:
		got = "string"
	case cursors.BooleanArrayCursor:
		got = "boolean"
	default:
		got = "invalid"
	}
	return fmt.Sprintf("schema collision: cannot group %s and %s types together", err.typ, got)
}

type storageTable interface {
	flux.Table
	Close()
	Cancel()
	Statistics() cursors.CursorStats
}

type storeReader struct {
	s storage.Store
}

// NewReader returns a new storageflux reader
func NewReader(s storage.Store) query.StorageReader {
	return &storeReader{s: s}
}

func (r *storeReader) ReadFilter(ctx context.Context, spec query.ReadFilterSpec, alloc *memory.Allocator) (query.TableIterator, error) {
	return &filterIterator{
		ctx:   ctx,
		s:     r.s,
		spec:  spec,
		cache: newTagsCache(0),
		alloc: alloc,
	}, nil
}

func (r *storeReader) GetGroupCapability(ctx context.Context) query.GroupCapability {
	if aggStore, ok := r.s.(storage.GroupStore); ok {
		return aggStore.GetGroupCapability(ctx)
	}
	return nil
}

func (r *storeReader) ReadGroup(ctx context.Context, spec query.ReadGroupSpec, alloc *memory.Allocator) (query.TableIterator, error) {
	return &groupIterator{
		ctx:   ctx,
		s:     r.s,
		spec:  spec,
		cache: newTagsCache(0),
		alloc: alloc,
	}, nil
}

func (r *storeReader) GetWindowAggregateCapability(ctx context.Context) query.WindowAggregateCapability {
	if aggStore, ok := r.s.(storage.WindowAggregateStore); ok {
		return aggStore.GetWindowAggregateCapability(ctx)
	}
	return nil
}

func (r *storeReader) ReadWindowAggregate(ctx context.Context, spec query.ReadWindowAggregateSpec, alloc *memory.Allocator) (query.TableIterator, error) {
	return &windowAggregateIterator{
		ctx:   ctx,
		s:     r.s,
		spec:  spec,
		cache: newTagsCache(0),
		alloc: alloc,
	}, nil
}

func (r *storeReader) ReadTagKeys(ctx context.Context, spec query.ReadTagKeysSpec, alloc *memory.Allocator) (query.TableIterator, error) {
	return &tagKeysIterator{
		ctx:       ctx,
		bounds:    spec.Bounds,
		s:         r.s,
		readSpec:  spec,
		predicate: spec.Predicate,
		alloc:     alloc,
	}, nil
}

func (r *storeReader) ReadTagValues(ctx context.Context, spec query.ReadTagValuesSpec, alloc *memory.Allocator) (query.TableIterator, error) {
	return &tagValuesIterator{
		ctx:       ctx,
		bounds:    spec.Bounds,
		s:         r.s,
		readSpec:  spec,
		predicate: spec.Predicate,
		alloc:     alloc,
	}, nil
}

func (r *storeReader) Close() {}

type filterIterator struct {
	ctx   context.Context
	s     storage.Store
	spec  query.ReadFilterSpec
	stats cursors.CursorStats
	cache *tagsCache
	alloc *memory.Allocator
}

func (fi *filterIterator) Statistics() cursors.CursorStats { return fi.stats }

func (fi *filterIterator) Do(f func(flux.Table) error) error {
	src := fi.s.GetSource(
		uint64(fi.spec.OrganizationID),
		uint64(fi.spec.BucketID),
	)

	// Setup read request
	any, err := types.MarshalAny(src)
	if err != nil {
		return err
	}

	var req datatypes.ReadFilterRequest
	req.ReadSource = any
	req.Predicate = fi.spec.Predicate
	req.Range.Start = int64(fi.spec.Bounds.Start)
	req.Range.End = int64(fi.spec.Bounds.Stop)

	rs, err := fi.s.ReadFilter(fi.ctx, &req)
	if err != nil {
		return err
	}

	if rs == nil {
		return nil
	}

	return fi.handleRead(f, rs)
}

func (fi *filterIterator) handleRead(f func(flux.Table) error, rs storage.ResultSet) error {
	// these resources must be closed if not nil on return
	var (
		cur   cursors.Cursor
		table storageTable
	)

	defer func() {
		if table != nil {
			table.Close()
		}
		if cur != nil {
			cur.Close()
		}
		rs.Close()
		fi.cache.Release()
	}()

READ:
	for rs.Next() {
		cur = rs.Cursor()
		if cur == nil {
			// no data for series key + field combination
			continue
		}

		bnds := fi.spec.Bounds
		key := defaultGroupKeyForSeries(rs.Tags(), bnds)
		done := make(chan struct{})
		switch typedCur := cur.(type) {
		case cursors.IntegerArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TInt)
			table = newIntegerTable(done, typedCur, bnds, key, cols, rs.Tags(), defs, fi.cache, fi.alloc)
		case cursors.FloatArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TFloat)
			table = newFloatTable(done, typedCur, bnds, key, cols, rs.Tags(), defs, fi.cache, fi.alloc)
		case cursors.UnsignedArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TUInt)
			table = newUnsignedTable(done, typedCur, bnds, key, cols, rs.Tags(), defs, fi.cache, fi.alloc)
		case cursors.BooleanArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TBool)
			table = newBooleanTable(done, typedCur, bnds, key, cols, rs.Tags(), defs, fi.cache, fi.alloc)
		case cursors.StringArrayCursor:
			cols, defs := determineTableColsForSeries(rs.Tags(), flux.TString)
			table = newStringTable(done, typedCur, bnds, key, cols, rs.Tags(), defs, fi.cache, fi.alloc)
		default:
			panic(fmt.Sprintf("unreachable: %T", typedCur))
		}

		cur = nil

		if !table.Empty() {
			if err := f(table); err != nil {
				table.Close()
				table = nil
				return err
			}
			select {
			case <-done:
			case <-fi.ctx.Done():
				table.Cancel()
				break READ
			}
		}

		stats := table.Statistics()
		fi.stats.ScannedValues += stats.ScannedValues
		fi.stats.ScannedBytes += stats.ScannedBytes
		table.Close()
		table = nil
	}
	return rs.Err()
}

type groupIterator struct {
	ctx   context.Context
	s     storage.Store
	spec  query.ReadGroupSpec
	stats cursors.CursorStats
	cache *tagsCache
	alloc *memory.Allocator
}

func (gi *groupIterator) Statistics() cursors.CursorStats { return gi.stats }

func (gi *groupIterator) Do(f func(flux.Table) error) error {
	src := gi.s.GetSource(
		uint64(gi.spec.OrganizationID),
		uint64(gi.spec.BucketID),
	)

	// Setup read request
	any, err := types.MarshalAny(src)
	if err != nil {
		return err
	}

	var req datatypes.ReadGroupRequest
	req.ReadSource = any
	req.Predicate = gi.spec.Predicate
	req.Range.Start = int64(gi.spec.Bounds.Start)
	req.Range.End = int64(gi.spec.Bounds.Stop)

	req.Group = convertGroupMode(gi.spec.GroupMode)
	req.GroupKeys = gi.spec.GroupKeys

	if agg, err := determineAggregateMethod(gi.spec.AggregateMethod); err != nil {
		return err
	} else if agg != datatypes.AggregateTypeNone {
		req.Aggregate = &datatypes.Aggregate{Type: agg}
	}

	rs, err := gi.s.ReadGroup(gi.ctx, &req)
	if err != nil {
		return err
	}

	if rs == nil {
		return nil
	}
	return gi.handleRead(f, rs)
}

func (gi *groupIterator) handleRead(f func(flux.Table) error, rs storage.GroupResultSet) error {
	// these resources must be closed if not nil on return
	var (
		gc    storage.GroupCursor
		cur   cursors.Cursor
		table storageTable
	)

	defer func() {
		if table != nil {
			table.Close()
		}
		if cur != nil {
			cur.Close()
		}
		if gc != nil {
			gc.Close()
		}
		rs.Close()
		gi.cache.Release()
	}()

	gc = rs.Next()
READ:
	for gc != nil {
		for gc.Next() {
			cur = gc.Cursor()
			if cur != nil {
				break
			}
		}

		if cur == nil {
			gc.Close()
			gc = rs.Next()
			continue
		}

		bnds := gi.spec.Bounds
		key := groupKeyForGroup(gc.PartitionKeyVals(), &gi.spec, bnds)
		done := make(chan struct{})
		switch typedCur := cur.(type) {
		case cursors.IntegerArrayCursor:
			cols, defs := determineTableColsForGroup(gc.Keys(), flux.TInt)
			table = newIntegerGroupTable(done, gc, typedCur, bnds, key, cols, gc.Tags(), defs, gi.cache, gi.alloc)
		case cursors.FloatArrayCursor:
			cols, defs := determineTableColsForGroup(gc.Keys(), flux.TFloat)
			table = newFloatGroupTable(done, gc, typedCur, bnds, key, cols, gc.Tags(), defs, gi.cache, gi.alloc)
		case cursors.UnsignedArrayCursor:
			cols, defs := determineTableColsForGroup(gc.Keys(), flux.TUInt)
			table = newUnsignedGroupTable(done, gc, typedCur, bnds, key, cols, gc.Tags(), defs, gi.cache, gi.alloc)
		case cursors.BooleanArrayCursor:
			cols, defs := determineTableColsForGroup(gc.Keys(), flux.TBool)
			table = newBooleanGroupTable(done, gc, typedCur, bnds, key, cols, gc.Tags(), defs, gi.cache, gi.alloc)
		case cursors.StringArrayCursor:
			cols, defs := determineTableColsForGroup(gc.Keys(), flux.TString)
			table = newStringGroupTable(done, gc, typedCur, bnds, key, cols, gc.Tags(), defs, gi.cache, gi.alloc)
		default:
			panic(fmt.Sprintf("unreachable: %T", typedCur))
		}

		// table owns these resources and is responsible for closing them
		cur = nil
		gc = nil

		if err := f(table); err != nil {
			table.Close()
			table = nil
			return err
		}
		select {
		case <-done:
		case <-gi.ctx.Done():
			table.Cancel()
			break READ
		}

		stats := table.Statistics()
		gi.stats.ScannedValues += stats.ScannedValues
		gi.stats.ScannedBytes += stats.ScannedBytes
		table.Close()
		table = nil

		gc = rs.Next()
	}
	return rs.Err()
}

func determineAggregateMethod(agg string) (datatypes.Aggregate_AggregateType, error) {
	if agg == "" {
		return datatypes.AggregateTypeNone, nil
	}

	if t, ok := datatypes.Aggregate_AggregateType_value[strings.ToUpper(agg)]; ok {
		return datatypes.Aggregate_AggregateType(t), nil
	}
	return 0, fmt.Errorf("unknown aggregate type %q", agg)
}

func convertGroupMode(m query.GroupMode) datatypes.ReadGroupRequest_Group {
	switch m {
	case query.GroupModeNone:
		return datatypes.GroupNone
	case query.GroupModeBy:
		return datatypes.GroupBy
	}
	panic(fmt.Sprint("invalid group mode: ", m))
}

const (
	startColIdx         = 0
	stopColIdx          = 1
	timeColIdx          = 2
	windowedValueColIdx = 2
	valueColIdx         = 3
)

func determineTableColsForWindowAggregate(tags models.Tags, typ flux.ColType) ([]flux.ColMeta, [][]byte) {
	var cols []flux.ColMeta
	var defs [][]byte

	// aggregates remove the _time column
	size := 3
	cols = make([]flux.ColMeta, size+len(tags))
	defs = make([][]byte, size+len(tags))
	cols[startColIdx] = flux.ColMeta{
		Label: execute.DefaultStartColLabel,
		Type:  flux.TTime,
	}
	cols[stopColIdx] = flux.ColMeta{
		Label: execute.DefaultStopColLabel,
		Type:  flux.TTime,
	}
	cols[windowedValueColIdx] = flux.ColMeta{
		Label: execute.DefaultValueColLabel,
		Type:  typ,
	}
	for j, tag := range tags {
		cols[size+j] = flux.ColMeta{
			Label: string(tag.Key),
			Type:  flux.TString,
		}
		defs[size+j] = []byte("")
	}
	return cols, defs
}

func determineTableColsForSeries(tags models.Tags, typ flux.ColType) ([]flux.ColMeta, [][]byte) {
	cols := make([]flux.ColMeta, 4+len(tags))
	defs := make([][]byte, 4+len(tags))
	cols[startColIdx] = flux.ColMeta{
		Label: execute.DefaultStartColLabel,
		Type:  flux.TTime,
	}
	cols[stopColIdx] = flux.ColMeta{
		Label: execute.DefaultStopColLabel,
		Type:  flux.TTime,
	}
	cols[timeColIdx] = flux.ColMeta{
		Label: execute.DefaultTimeColLabel,
		Type:  flux.TTime,
	}
	cols[valueColIdx] = flux.ColMeta{
		Label: execute.DefaultValueColLabel,
		Type:  typ,
	}
	for j, tag := range tags {
		cols[4+j] = flux.ColMeta{
			Label: string(tag.Key),
			Type:  flux.TString,
		}
		defs[4+j] = []byte("")
	}
	return cols, defs
}

func defaultGroupKeyForSeries(tags models.Tags, bnds execute.Bounds) flux.GroupKey {
	cols := make([]flux.ColMeta, 2, len(tags)+2)
	vs := make([]values.Value, 2, len(tags)+2)
	cols[startColIdx] = flux.ColMeta{
		Label: execute.DefaultStartColLabel,
		Type:  flux.TTime,
	}
	vs[startColIdx] = values.NewTime(bnds.Start)
	cols[stopColIdx] = flux.ColMeta{
		Label: execute.DefaultStopColLabel,
		Type:  flux.TTime,
	}
	vs[stopColIdx] = values.NewTime(bnds.Stop)
	for i := range tags {
		cols = append(cols, flux.ColMeta{
			Label: string(tags[i].Key),
			Type:  flux.TString,
		})
		vs = append(vs, values.NewString(string(tags[i].Value)))
	}
	return execute.NewGroupKey(cols, vs)
}

func determineTableColsForGroup(tagKeys [][]byte, typ flux.ColType) ([]flux.ColMeta, [][]byte) {
	cols := make([]flux.ColMeta, 4+len(tagKeys))
	defs := make([][]byte, 4+len(tagKeys))
	cols[startColIdx] = flux.ColMeta{
		Label: execute.DefaultStartColLabel,
		Type:  flux.TTime,
	}
	cols[stopColIdx] = flux.ColMeta{
		Label: execute.DefaultStopColLabel,
		Type:  flux.TTime,
	}
	cols[timeColIdx] = flux.ColMeta{
		Label: execute.DefaultTimeColLabel,
		Type:  flux.TTime,
	}
	cols[valueColIdx] = flux.ColMeta{
		Label: execute.DefaultValueColLabel,
		Type:  typ,
	}
	for j, tag := range tagKeys {
		cols[4+j] = flux.ColMeta{
			Label: string(tag),
			Type:  flux.TString,
		}
		defs[4+j] = []byte("")

	}
	return cols, defs
}

func groupKeyForGroup(kv [][]byte, spec *query.ReadGroupSpec, bnds execute.Bounds) flux.GroupKey {
	cols := make([]flux.ColMeta, 2, len(spec.GroupKeys)+2)
	vs := make([]values.Value, 2, len(spec.GroupKeys)+2)
	cols[startColIdx] = flux.ColMeta{
		Label: execute.DefaultStartColLabel,
		Type:  flux.TTime,
	}
	vs[startColIdx] = values.NewTime(bnds.Start)
	cols[stopColIdx] = flux.ColMeta{
		Label: execute.DefaultStopColLabel,
		Type:  flux.TTime,
	}
	vs[stopColIdx] = values.NewTime(bnds.Stop)
	for i := range spec.GroupKeys {
		if spec.GroupKeys[i] == execute.DefaultStartColLabel || spec.GroupKeys[i] == execute.DefaultStopColLabel {
			continue
		}
		cols = append(cols, flux.ColMeta{
			Label: spec.GroupKeys[i],
			Type:  flux.TString,
		})
		vs = append(vs, values.NewString(string(kv[i])))
	}
	return execute.NewGroupKey(cols, vs)
}

type windowAggregateIterator struct {
	ctx   context.Context
	s     storage.Store
	spec  query.ReadWindowAggregateSpec
	stats cursors.CursorStats
	cache *tagsCache
	alloc *memory.Allocator
}

func (wai *windowAggregateIterator) Statistics() cursors.CursorStats { return wai.stats }

func (wai *windowAggregateIterator) Do(f func(flux.Table) error) error {
	src := wai.s.GetSource(
		uint64(wai.spec.OrganizationID),
		uint64(wai.spec.BucketID),
	)

	// Setup read request
	any, err := types.MarshalAny(src)
	if err != nil {
		return err
	}

	var req datatypes.ReadWindowAggregateRequest
	req.ReadSource = any
	req.Predicate = wai.spec.Predicate
	req.Range.Start = int64(wai.spec.Bounds.Start)
	req.Range.End = int64(wai.spec.Bounds.Stop)

	req.WindowEvery = wai.spec.WindowEvery
	req.Aggregate = make([]*datatypes.Aggregate, len(wai.spec.Aggregates))

	for i, aggKind := range wai.spec.Aggregates {
		if agg, err := determineAggregateMethod(string(aggKind)); err != nil {
			return err
		} else if agg != datatypes.AggregateTypeNone {
			req.Aggregate[i] = &datatypes.Aggregate{Type: agg}
		}
	}

	aggStore, ok := wai.s.(storage.WindowAggregateStore)
	if !ok {
		return errors.New("storage does not support window aggregate")
	}
	rs, err := aggStore.WindowAggregate(wai.ctx, &req)
	if err != nil {
		return err
	}

	if rs == nil {
		return nil
	}
	return wai.handleRead(f, rs)
}

func (wai *windowAggregateIterator) handleRead(f func(flux.Table) error, rs storage.ResultSet) error {
	windowEvery := wai.spec.WindowEvery
	createEmpty := wai.spec.CreateEmpty

	// these resources must be closed if not nil on return
	var (
		cur   cursors.Cursor
		table storageTable
	)

	defer func() {
		if table != nil {
			table.Close()
		}
		if cur != nil {
			cur.Close()
		}
		rs.Close()
		wai.cache.Release()
	}()

READ:
	for rs.Next() {
		cur = rs.Cursor()
		if cur == nil {
			// no data for series key + field combination
			continue
		}

		bnds := wai.spec.Bounds
		key := defaultGroupKeyForSeries(rs.Tags(), bnds)
		done := make(chan struct{})
		switch typedCur := cur.(type) {
		case cursors.IntegerArrayCursor:
			cols, defs := determineTableColsForWindowAggregate(rs.Tags(), flux.TInt)
			table = newIntegerWindowTable(done, typedCur, bnds, windowEvery, createEmpty, key, cols, rs.Tags(), defs, wai.cache, wai.alloc)
		case cursors.FloatArrayCursor:
			cols, defs := determineTableColsForWindowAggregate(rs.Tags(), flux.TFloat)
			table = newFloatWindowTable(done, typedCur, bnds, windowEvery, createEmpty, key, cols, rs.Tags(), defs, wai.cache, wai.alloc)
		case cursors.UnsignedArrayCursor:
			cols, defs := determineTableColsForWindowAggregate(rs.Tags(), flux.TUInt)
			table = newUnsignedWindowTable(done, typedCur, bnds, windowEvery, createEmpty, key, cols, rs.Tags(), defs, wai.cache, wai.alloc)
		case cursors.BooleanArrayCursor:
			cols, defs := determineTableColsForWindowAggregate(rs.Tags(), flux.TBool)
			table = newBooleanWindowTable(done, typedCur, bnds, windowEvery, createEmpty, key, cols, rs.Tags(), defs, wai.cache, wai.alloc)
		case cursors.StringArrayCursor:
			cols, defs := determineTableColsForWindowAggregate(rs.Tags(), flux.TString)
			table = newStringWindowTable(done, typedCur, bnds, windowEvery, createEmpty, key, cols, rs.Tags(), defs, wai.cache, wai.alloc)
		default:
			panic(fmt.Sprintf("unreachable: %T", typedCur))
		}

		cur = nil

		if !table.Empty() {
			if err := splitWindows(wai.ctx, table, f); err != nil {
				table.Close()
				table = nil
				return err
			}
			select {
			case <-done:
			case <-wai.ctx.Done():
				table.Cancel()
				break READ
			}
		}

		stats := table.Statistics()
		wai.stats.ScannedValues += stats.ScannedValues
		wai.stats.ScannedBytes += stats.ScannedBytes
		table.Close()
		table = nil
	}
	return rs.Err()
}

type tagKeysIterator struct {
	ctx       context.Context
	bounds    execute.Bounds
	s         storage.Store
	readSpec  query.ReadTagKeysSpec
	predicate *datatypes.Predicate
	alloc     *memory.Allocator
}

func (ti *tagKeysIterator) Do(f func(flux.Table) error) error {
	src := ti.s.GetSource(
		uint64(ti.readSpec.OrganizationID),
		uint64(ti.readSpec.BucketID),
	)

	var req datatypes.TagKeysRequest
	any, err := types.MarshalAny(src)
	if err != nil {
		return err
	}

	req.TagsSource = any
	req.Predicate = ti.predicate
	req.Range.Start = int64(ti.bounds.Start)
	req.Range.End = int64(ti.bounds.Stop)

	rs, err := ti.s.TagKeys(ti.ctx, &req)
	if err != nil {
		return err
	}
	return ti.handleRead(f, rs)
}

func (ti *tagKeysIterator) handleRead(f func(flux.Table) error, rs cursors.StringIterator) error {
	key := execute.NewGroupKey(nil, nil)
	builder := execute.NewColListTableBuilder(key, ti.alloc)
	valueIdx, err := builder.AddCol(flux.ColMeta{
		Label: execute.DefaultValueColLabel,
		Type:  flux.TString,
	})
	if err != nil {
		return err
	}
	defer builder.ClearData()

	// Add the _start and _stop columns that come from storage.
	if err := builder.AppendString(valueIdx, "_start"); err != nil {
		return err
	}
	if err := builder.AppendString(valueIdx, "_stop"); err != nil {
		return err
	}

	for rs.Next() {
		v := rs.Value()
		switch v {
		case models.MeasurementTagKey:
			v = "_measurement"
		case models.FieldKeyTagKey:
			v = "_field"
		}

		if err := builder.AppendString(valueIdx, v); err != nil {
			return err
		}
	}

	// Construct the table and add to the reference count
	// so we can free the table later.
	tbl, err := builder.Table()
	if err != nil {
		return err
	}

	// Release the references to the arrays held by the builder.
	builder.ClearData()
	return f(tbl)
}

func (ti *tagKeysIterator) Statistics() cursors.CursorStats {
	return cursors.CursorStats{}
}

type tagValuesIterator struct {
	ctx       context.Context
	bounds    execute.Bounds
	s         storage.Store
	readSpec  query.ReadTagValuesSpec
	predicate *datatypes.Predicate
	alloc     *memory.Allocator
}

func (ti *tagValuesIterator) Do(f func(flux.Table) error) error {
	src := ti.s.GetSource(
		uint64(ti.readSpec.OrganizationID),
		uint64(ti.readSpec.BucketID),
	)

	var req datatypes.TagValuesRequest
	any, err := types.MarshalAny(src)
	if err != nil {
		return err
	}
	req.TagsSource = any

	switch ti.readSpec.TagKey {
	case "_measurement":
		req.TagKey = models.MeasurementTagKey
	case "_field":
		req.TagKey = models.FieldKeyTagKey
	default:
		req.TagKey = ti.readSpec.TagKey
	}
	req.Predicate = ti.predicate
	req.Range.Start = int64(ti.bounds.Start)
	req.Range.End = int64(ti.bounds.Stop)

	rs, err := ti.s.TagValues(ti.ctx, &req)
	if err != nil {
		return err
	}
	return ti.handleRead(f, rs)
}

func (ti *tagValuesIterator) handleRead(f func(flux.Table) error, rs cursors.StringIterator) error {
	key := execute.NewGroupKey(nil, nil)
	builder := execute.NewColListTableBuilder(key, ti.alloc)
	valueIdx, err := builder.AddCol(flux.ColMeta{
		Label: execute.DefaultValueColLabel,
		Type:  flux.TString,
	})
	if err != nil {
		return err
	}
	defer builder.ClearData()

	for rs.Next() {
		if err := builder.AppendString(valueIdx, rs.Value()); err != nil {
			return err
		}
	}

	// Construct the table and add to the reference count
	// so we can free the table later.
	tbl, err := builder.Table()
	if err != nil {
		return err
	}

	// Release the references to the arrays held by the builder.
	builder.ClearData()
	return f(tbl)
}

func (ti *tagValuesIterator) Statistics() cursors.CursorStats {
	return cursors.CursorStats{}
}
