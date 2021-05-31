package clickhouseReader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	_ "github.com/ClickHouse/clickhouse-go"
	"github.com/jmoiron/sqlx"

	"go.signoz.io/query-service/model"
	"go.uber.org/zap"
)

const (
	primaryNamespace = "clickhouse"
	archiveNamespace = "clickhouse-archive"

	minTimespanForProgressiveSearch       = time.Hour
	minTimespanForProgressiveSearchMargin = time.Minute
	maxProgressiveSteps                   = 4
)

var (
	ErrNoOperationsTable = errors.New("no operations table supplied")
	ErrNoIndexTable      = errors.New("no index table supplied")
	ErrStartTimeRequired = errors.New("start time is required for search queries")
)

// SpanWriter for reading spans from ClickHouse
type ClickHouseReader struct {
	db              *sqlx.DB
	operationsTable string
	indexTable      string
	spansTable      string
}

// NewTraceReader returns a TraceReader for the database
func NewReader() *ClickHouseReader {

	datasource := os.Getenv("ClickHouseUrl")
	options := NewOptions(datasource, primaryNamespace, archiveNamespace)
	db, err := initialize(options)

	if err != nil {
		zap.S().Error(err)
	}
	return &ClickHouseReader{
		db:              db,
		operationsTable: options.primary.OperationsTable,
		indexTable:      options.primary.IndexTable,
		spansTable:      options.primary.SpansTable,
	}
}

func initialize(options *Options) (*sqlx.DB, error) {

	db, err := connect(options.getPrimary())
	if err != nil {
		return nil, fmt.Errorf("error connecting to primary db: %v", err)
	}

	return db, nil
}

func connect(cfg *namespaceConfig) (*sqlx.DB, error) {
	if cfg.Encoding != EncodingJSON && cfg.Encoding != EncodingProto {
		return nil, fmt.Errorf("unknown encoding %q, supported: %q, %q", cfg.Encoding, EncodingJSON, EncodingProto)
	}

	return cfg.Connector(cfg)
}

func (r *ClickHouseReader) getStrings(ctx context.Context, sql string, args ...interface{}) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	values := []string{}

	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return values, nil
}

// GetServices fetches the sorted service list that have not expired
func (r *ClickHouseReader) GetServices(ctx context.Context, queryParams *model.GetServicesParams) (*[]model.ServiceItem, error) {

	if r.indexTable == "" {
		return nil, ErrNoIndexTable
	}

	var serviceItems []model.ServiceItem

	query := fmt.Sprintf("SELECT serviceName, quantile(0.99)(durationNano) as p99, avg(durationNano) as avgDuration, count(*) as numCalls FROM %s WHERE timestamp>='%s' AND timestamp<='%s' AND kind='2' GROUP BY serviceName", r.indexTable, strconv.FormatInt(queryParams.Start.UnixNano(), 10), strconv.FormatInt(queryParams.End.UnixNano(), 10))
	err := r.db.Select(&serviceItems, query)

	zap.S().Info(query)

	if err != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}

	if serviceItems == nil {
		serviceItems = []model.ServiceItem{}
	}

	return &serviceItems, nil
}

func (r *ClickHouseReader) GetServiceOverview(ctx context.Context, queryParams *model.GetServiceOverviewParams) (*[]model.ServiceOverviewItem, error) {

	var serviceOverviewItems []model.ServiceOverviewItem

	query := fmt.Sprintf("SELECT toStartOfInterval(timestamp, INTERVAL %s minute) as time, quantile(0.99)(durationNano) as p99, quantile(0.95)(durationNano) as p95,quantile(0.50)(durationNano) as p50, count(*) as numCalls FROM %s WHERE timestamp>='%s' AND timestamp<='%s' AND kind='2' AND serviceName='%s' GROUP BY time ORDER BY time DESC", strconv.Itoa(int(queryParams.StepSeconds/60)), r.indexTable, strconv.FormatInt(queryParams.Start.UnixNano(), 10), strconv.FormatInt(queryParams.End.UnixNano(), 10), queryParams.ServiceName)

	err := r.db.Select(&serviceOverviewItems, query)

	zap.S().Info(query)

	if err != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}

	for i, _ := range serviceOverviewItems {
		timeObj, _ := time.Parse(time.RFC3339Nano, serviceOverviewItems[i].Time)
		serviceOverviewItems[i].Timestamp = int64(timeObj.UnixNano())
		serviceOverviewItems[i].Time = ""
	}

	if serviceOverviewItems == nil {
		serviceOverviewItems = []model.ServiceOverviewItem{}
	}

	return &serviceOverviewItems, nil

}

func (r *ClickHouseReader) SearchSpans(ctx context.Context, queryParams *model.SpanSearchParams) (*[]model.SearchSpansResult, error) {

	query := fmt.Sprintf("SELECT timestamp, spanID, traceID, serviceName, name, kind, durationNano, tagsKeys, tagsValues FROM %s WHERE timestamp >= ? AND timestamp <= ?", r.indexTable)

	args := []interface{}{strconv.FormatInt(queryParams.Start.UnixNano(), 10), strconv.FormatInt(queryParams.End.UnixNano(), 10)}

	if len(queryParams.ServiceName) != 0 {
		query = query + " AND serviceName = ?"
		args = append(args, queryParams.ServiceName)
	}

	if len(queryParams.OperationName) != 0 {

		query = query + " AND name = ?"
		args = append(args, queryParams.OperationName)

	}

	if len(queryParams.Kind) != 0 {
		query = query + " AND kind = ?"
		args = append(args, queryParams.Kind)

	}

	if len(queryParams.MinDuration) != 0 {
		query = query + " AND durationNano >= ?"
		args = append(args, queryParams.MinDuration)
	}
	if len(queryParams.MaxDuration) != 0 {
		query = query + " AND durationNano <= ?"
		args = append(args, queryParams.MaxDuration)
	}

	for _, item := range queryParams.Tags {

		if item.Operator == "equals" {
			query = query + " AND has(tags, ?)"
			args = append(args, fmt.Sprintf("%s:%s", item.Key, item.Value))

		} else if item.Operator == "contains" {
			query = query + " AND tagsValues[indexOf(tagsKeys, ?)] ILIKE ?"
			args = append(args, item.Key)
			args = append(args, fmt.Sprintf("%%%s%%", item.Value))
		} else if item.Operator == "isnotnull" {
			query = query + " AND has(tagsKeys, ?)"
			args = append(args, item.Key)
		} else {
			return nil, fmt.Errorf("Tag Operator %s not supported", item.Operator)
		}

		if item.Key == "error" && item.Value == "true" {
			query = query + " OR statusCode>=500"
		}
		query = query + " ORDER BY timestamp LIMIT 100"

	}

	var searchScanReponses []model.SearchSpanReponseItem

	err := r.db.Select(&searchScanReponses, query, args...)

	zap.S().Info(query)

	if err != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}

	searchSpansResult := []model.SearchSpansResult{
		model.SearchSpansResult{
			Columns: []string{"__time", "SpanId", "TraceId", "ServiceName", "Name", "Kind", "DurationNano", "TagsKeys", "TagsValues"},
			Events:  make([][]interface{}, len(searchScanReponses)),
		},
	}

	for i, item := range searchScanReponses {
		spanEvents := item.GetValues()
		searchSpansResult[0].Events[i] = spanEvents
	}

	return &searchSpansResult, nil
}

func (r *ClickHouseReader) GetServiceDBOverview(ctx context.Context, queryParams *model.GetServiceOverviewParams) (*[]model.ServiceDBOverviewItem, error) {

	var serviceDBOverviewItems []model.ServiceDBOverviewItem

	query := fmt.Sprintf("SELECT toStartOfInterval(timestamp, INTERVAL %s minute) as time, avg(durationNano) as avgDuration, count(1) as numCalls, dbSystem FROM %s WHERE serviceName='%s' AND timestamp>='%s' AND timestamp<='%s' AND kind='3' AND dbName IS NOT NULL GROUP BY time, dbSystem ORDER BY time DESC", strconv.Itoa(int(queryParams.StepSeconds/60)), r.indexTable, queryParams.ServiceName, strconv.FormatInt(queryParams.Start.UnixNano(), 10), strconv.FormatInt(queryParams.End.UnixNano(), 10))

	err := r.db.Select(&serviceDBOverviewItems, query)

	zap.S().Info(query)

	if err != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}

	for i, _ := range serviceDBOverviewItems {
		timeObj, _ := time.Parse(time.RFC3339Nano, serviceDBOverviewItems[i].Time)
		serviceDBOverviewItems[i].Timestamp = int64(timeObj.UnixNano())
		serviceDBOverviewItems[i].Time = ""
		serviceDBOverviewItems[i].CallRate = float32(serviceDBOverviewItems[i].NumCalls) / float32(queryParams.StepSeconds)
	}

	if serviceDBOverviewItems == nil {
		serviceDBOverviewItems = []model.ServiceDBOverviewItem{}
	}

	return &serviceDBOverviewItems, nil

}

func (r *ClickHouseReader) GetServiceExternalAvgDuration(ctx context.Context, queryParams *model.GetServiceOverviewParams) (*[]model.ServiceExternalItem, error) {

	var serviceExternalItems []model.ServiceExternalItem

	query := fmt.Sprintf("SELECT toStartOfInterval(timestamp, INTERVAL %s minute) as time, avg(durationNano) as avgDuration FROM %s WHERE serviceName='%s' AND timestamp>='%s' AND timestamp<='%s' AND kind='3' AND externalHttpUrl IS NOT NULL GROUP BY time ORDER BY time DESC", strconv.Itoa(int(queryParams.StepSeconds/60)), r.indexTable, queryParams.ServiceName, strconv.FormatInt(queryParams.Start.UnixNano(), 10), strconv.FormatInt(queryParams.End.UnixNano(), 10))

	err := r.db.Select(&serviceExternalItems, query)

	zap.S().Info(query)

	if err != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}

	for i, _ := range serviceExternalItems {
		timeObj, _ := time.Parse(time.RFC3339Nano, serviceExternalItems[i].Time)
		serviceExternalItems[i].Timestamp = int64(timeObj.UnixNano())
		serviceExternalItems[i].Time = ""
		serviceExternalItems[i].CallRate = float32(serviceExternalItems[i].NumCalls) / float32(queryParams.StepSeconds)
	}

	if serviceExternalItems == nil {
		serviceExternalItems = []model.ServiceExternalItem{}
	}

	return &serviceExternalItems, nil
}

func (r *ClickHouseReader) GetServiceExternalErrors(ctx context.Context, queryParams *model.GetServiceOverviewParams) (*[]model.ServiceExternalItem, error) {

	var serviceExternalErrorItems []model.ServiceExternalItem

	query := fmt.Sprintf("SELECT toStartOfInterval(timestamp, INTERVAL %s minute) as time, avg(durationNano) as avgDuration, count(1) as numCalls, externalHttpUrl FROM %s WHERE serviceName='%s' AND timestamp>='%s' AND timestamp<='%s' AND kind='3' AND externalHttpUrl IS NOT NULL AND statusCode >= 500 GROUP BY time, externalHttpUrl ORDER BY time DESC", strconv.Itoa(int(queryParams.StepSeconds/60)), r.indexTable, queryParams.ServiceName, strconv.FormatInt(queryParams.Start.UnixNano(), 10), strconv.FormatInt(queryParams.End.UnixNano(), 10))

	err := r.db.Select(&serviceExternalErrorItems, query)

	zap.S().Info(query)

	if err != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}
	var serviceExternalTotalItems []model.ServiceExternalItem

	queryTotal := fmt.Sprintf("SELECT toStartOfInterval(timestamp, INTERVAL %s minute) as time, avg(durationNano) as avgDuration, count(1) as numCalls, externalHttpUrl FROM %s WHERE serviceName='%s' AND timestamp>='%s' AND timestamp<='%s' AND kind='3' AND externalHttpUrl IS NOT NULL GROUP BY time, externalHttpUrl ORDER BY time DESC", strconv.Itoa(int(queryParams.StepSeconds/60)), r.indexTable, queryParams.ServiceName, strconv.FormatInt(queryParams.Start.UnixNano(), 10), strconv.FormatInt(queryParams.End.UnixNano(), 10))

	errTotal := r.db.Select(&serviceExternalTotalItems, queryTotal)

	if errTotal != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}

	m := make(map[string]int)

	for j, _ := range serviceExternalErrorItems {
		timeObj, _ := time.Parse(time.RFC3339Nano, serviceExternalErrorItems[j].Time)
		m[strconv.FormatInt(timeObj.UnixNano(), 10)+"-"+serviceExternalErrorItems[j].ExternalHttpUrl] = serviceExternalErrorItems[j].NumCalls
	}

	for i, _ := range serviceExternalTotalItems {
		timeObj, _ := time.Parse(time.RFC3339Nano, serviceExternalTotalItems[i].Time)
		serviceExternalTotalItems[i].Timestamp = int64(timeObj.UnixNano())
		serviceExternalTotalItems[i].Time = ""
		// serviceExternalTotalItems[i].CallRate = float32(serviceExternalTotalItems[i].NumCalls) / float32(queryParams.StepSeconds)

		if val, ok := m[strconv.FormatInt(serviceExternalTotalItems[i].Timestamp, 10)+"-"+serviceExternalTotalItems[i].ExternalHttpUrl]; ok {
			serviceExternalTotalItems[i].NumErrors = val
			serviceExternalTotalItems[i].ErrorRate = float32(serviceExternalTotalItems[i].NumErrors) * 100 / float32(serviceExternalTotalItems[i].NumCalls)
		}
		serviceExternalTotalItems[i].CallRate = 0
		serviceExternalTotalItems[i].NumCalls = 0

	}

	if serviceExternalTotalItems == nil {
		serviceExternalTotalItems = []model.ServiceExternalItem{}
	}

	return &serviceExternalTotalItems, nil
}

func (r *ClickHouseReader) GetServiceExternal(ctx context.Context, queryParams *model.GetServiceOverviewParams) (*[]model.ServiceExternalItem, error) {

	var serviceExternalItems []model.ServiceExternalItem

	query := fmt.Sprintf("SELECT toStartOfInterval(timestamp, INTERVAL %s minute) as time, avg(durationNano) as avgDuration, count(1) as numCalls, externalHttpUrl FROM %s WHERE serviceName='%s' AND timestamp>='%s' AND timestamp<='%s' AND kind='3' AND externalHttpUrl IS NOT NULL GROUP BY time, externalHttpUrl ORDER BY time DESC", strconv.Itoa(int(queryParams.StepSeconds/60)), r.indexTable, queryParams.ServiceName, strconv.FormatInt(queryParams.Start.UnixNano(), 10), strconv.FormatInt(queryParams.End.UnixNano(), 10))

	err := r.db.Select(&serviceExternalItems, query)

	zap.S().Info(query)

	if err != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}

	for i, _ := range serviceExternalItems {
		timeObj, _ := time.Parse(time.RFC3339Nano, serviceExternalItems[i].Time)
		serviceExternalItems[i].Timestamp = int64(timeObj.UnixNano())
		serviceExternalItems[i].Time = ""
		serviceExternalItems[i].CallRate = float32(serviceExternalItems[i].NumCalls) / float32(queryParams.StepSeconds)
	}

	if serviceExternalItems == nil {
		serviceExternalItems = []model.ServiceExternalItem{}
	}

	return &serviceExternalItems, nil
}

func (r *ClickHouseReader) GetTopEndpoints(ctx context.Context, queryParams *model.GetTopEndpointsParams) (*[]model.TopEndpointsItem, error) {

	var topEndpointsItems []model.TopEndpointsItem

	query := fmt.Sprintf("SELECT quantile(0.5)(durationNano) as p50, quantile(0.95)(durationNano) as p95, quantile(0.99)(durationNano) as p99, COUNT(1) as numCalls, name  FROM %s WHERE  timestamp >= '%s' AND timestamp <= '%s' AND  kind='2' and serviceName='%s' GROUP BY name", r.indexTable, strconv.FormatInt(queryParams.Start.UnixNano(), 10), strconv.FormatInt(queryParams.End.UnixNano(), 10), queryParams.ServiceName)

	err := r.db.Select(&topEndpointsItems, query)

	zap.S().Info(query)

	if err != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}

	if topEndpointsItems == nil {
		topEndpointsItems = []model.TopEndpointsItem{}
	}

	return &topEndpointsItems, nil
}

func (r *ClickHouseReader) GetUsage(ctx context.Context, queryParams *model.GetUsageParams) (*[]model.UsageItem, error) {

	var usageItems []model.UsageItem

	var query string
	if len(queryParams.ServiceName) != 0 {
		query = fmt.Sprintf("SELECT toStartOfInterval(timestamp, INTERVAL %d HOUR) as time, count(1) as count FROM %s WHERE serviceName='%s' AND timestamp>='%s' AND timestamp<='%s' GROUP BY time ORDER BY time ASC", queryParams.StepHour, r.indexTable, queryParams.ServiceName, strconv.FormatInt(queryParams.Start.UnixNano(), 10), strconv.FormatInt(queryParams.End.UnixNano(), 10))
	} else {
		query = fmt.Sprintf("SELECT toStartOfInterval(timestamp, INTERVAL %d HOUR) as time, count(1) as count FROM %s WHERE timestamp>='%s' AND timestamp<='%s' GROUP BY time ORDER BY time ASC", queryParams.StepHour, r.indexTable, strconv.FormatInt(queryParams.Start.UnixNano(), 10), strconv.FormatInt(queryParams.End.UnixNano(), 10))
	}

	err := r.db.Select(&usageItems, query)

	zap.S().Info(query)

	if err != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}

	for i, _ := range usageItems {
		timeObj, _ := time.Parse(time.RFC3339Nano, usageItems[i].Time)
		usageItems[i].Timestamp = int64(timeObj.UnixNano())
		usageItems[i].Time = ""
	}

	if usageItems == nil {
		usageItems = []model.UsageItem{}
	}

	return &usageItems, nil
}

func (r *ClickHouseReader) GetServicesList(ctx context.Context) (*[]string, error) {

	services := []string{}

	query := fmt.Sprintf(`SELECT DISTINCT serviceName FROM %s WHERE toDate(timestamp) > now() - INTERVAL 1 DAY`, r.indexTable)

	err := r.db.Select(&services, query)

	zap.S().Info(query)

	if err != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}

	return &services, nil
}

func (r *ClickHouseReader) GetTags(ctx context.Context, serviceName string) (*[]model.TagItem, error) {

	tagItems := []model.TagItem{}

	query := fmt.Sprintf(`SELECT DISTINCT arrayJoin(tagsKeys) as tagKeys FROM %s WHERE serviceName='%s'  AND toDate(timestamp) > now() - INTERVAL 1 DAY`, r.indexTable, serviceName)

	err := r.db.Select(&tagItems, query)

	zap.S().Info(query)

	if err != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}

	return &tagItems, nil
}

func (r *ClickHouseReader) GetOperations(ctx context.Context, serviceName string) (*[]string, error) {

	operations := []string{}

	query := fmt.Sprintf(`SELECT DISTINCT(name) FROM %s WHERE serviceName='%s'  AND toDate(timestamp) > now() - INTERVAL 1 DAY`, r.indexTable, serviceName)

	err := r.db.Select(&operations, query)

	zap.S().Info(query)

	if err != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}
	return &operations, nil
}

func (r *ClickHouseReader) SearchTraces(ctx context.Context, traceId string) (*[]model.SearchSpansResult, error) {

	var searchScanReponses []model.SearchSpanReponseItem

	query := fmt.Sprintf("SELECT timestamp, spanID, traceID, serviceName, name, kind, durationNano, tagsKeys, tagsValues, references FROM %s WHERE traceID='%s'", r.indexTable, traceId)

	err := r.db.Select(&searchScanReponses, query)

	zap.S().Info(query)

	if err != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}

	searchSpansResult := []model.SearchSpansResult{
		model.SearchSpansResult{
			Columns: []string{"__time", "SpanId", "TraceId", "ServiceName", "Name", "Kind", "DurationNano", "TagsKeys", "TagsValues", "References"},
			Events:  make([][]interface{}, len(searchScanReponses)),
		},
	}

	for i, item := range searchScanReponses {
		spanEvents := item.GetValues()
		searchSpansResult[0].Events[i] = spanEvents
	}

	return &searchSpansResult, nil

}
func (r *ClickHouseReader) GetServiceMapDependencies(ctx context.Context, queryParams *model.GetServicesParams) (*[]model.ServiceMapDependencyResponseItem, error) {
	serviceMapDependencyItems := []model.ServiceMapDependencyItem{}

	query := fmt.Sprintf(`SELECT spanID, parentSpanID, serviceName FROM %s WHERE timestamp>='%s' AND timestamp<='%s'`, r.indexTable, strconv.FormatInt(queryParams.Start.UnixNano(), 10), strconv.FormatInt(queryParams.End.UnixNano(), 10))

	err := r.db.Select(&serviceMapDependencyItems, query)

	zap.S().Info(query)

	if err != nil {
		zap.S().Debug("Error in processing sql query: ", err)
		return nil, fmt.Errorf("Error in processing sql query")
	}

	serviceMap := make(map[string]*model.ServiceMapDependencyResponseItem)

	spanId2ServiceNameMap := make(map[string]string)
	for i, _ := range serviceMapDependencyItems {
		spanId2ServiceNameMap[serviceMapDependencyItems[i].SpanId] = serviceMapDependencyItems[i].ServiceName
	}
	for i, _ := range serviceMapDependencyItems {
		parent2childServiceName := spanId2ServiceNameMap[serviceMapDependencyItems[i].ParentSpanId] + "-" + spanId2ServiceNameMap[serviceMapDependencyItems[i].SpanId]
		if _, ok := serviceMap[parent2childServiceName]; !ok {
			serviceMap[parent2childServiceName] = &model.ServiceMapDependencyResponseItem{
				Parent:    spanId2ServiceNameMap[serviceMapDependencyItems[i].ParentSpanId],
				Child:     spanId2ServiceNameMap[serviceMapDependencyItems[i].SpanId],
				CallCount: 1,
			}
		} else {
			serviceMap[parent2childServiceName].CallCount++
		}
	}

	retMe := make([]model.ServiceMapDependencyResponseItem, 0, len(serviceMap))
	for _, dependency := range serviceMap {
		if dependency.Parent == "" {
			continue
		}
		retMe = append(retMe, *dependency)
	}

	return &retMe, nil
}