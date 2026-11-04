package platform

import (
	"database/sql"
	"reflect"
	"time"

	"gorm.io/gorm"
)

// timedCandidateRows separates time spent advancing database/sql rows from
// GORM's field mapping. Next still includes driver decoding and result delivery.
type timedCandidateRows struct {
	*sql.Rows
	nextDuration time.Duration
}

func (rows *timedCandidateRows) Next() bool {
	started := time.Now()
	hasNext := rows.Rows.Next()
	rows.nextDuration += time.Since(started)
	return hasNext
}

func loadFeedCandidateRows(query *gorm.DB, scene string, metrics *serverMetrics) ([]Video, error) {
	totalStarted := time.Now()
	queryStarted := time.Now()
	rows, err := query.Rows()
	queryDuration := time.Since(queryStarted)
	metrics.observeFeedStageDuration(scene, "candidate_query_start", queryDuration, err)
	if err != nil {
		if rows != nil {
			_ = rows.Close()
		}
		metrics.observeFeedStage(scene, "candidate_sql", totalStarted, err)
		return nil, err
	}
	if rows == nil {
		err = gorm.ErrInvalidDB
		metrics.observeFeedStage(scene, "candidate_sql", totalStarted, err)
		return nil, err
	}

	items := make([]Video, 0)
	scanDB := query.Session(&gorm.Session{NewDB: true}).Model(&Video{})
	scanDB.Statement.Dest = &items
	scanDB.Statement.ReflectValue = reflect.ValueOf(&items).Elem()
	if err = scanDB.Statement.Parse(&Video{}); err == nil {
		timedRows := &timedCandidateRows{Rows: rows}
		scanStarted := time.Now()
		gorm.Scan(timedRows, scanDB, 0)
		scanDuration := time.Since(scanStarted)
		rowScanDuration := scanDuration - timedRows.nextDuration
		if rowScanDuration < 0 {
			rowScanDuration = 0
		}
		err = scanDB.Error
		if err == nil {
			err = rows.Err()
		}
		metrics.observeFeedStageDuration(scene, "candidate_row_next", timedRows.nextDuration, err)
		metrics.observeFeedStageDuration(scene, "candidate_row_scan", rowScanDuration, err)
	}
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	metrics.observeFeedStage(scene, "candidate_sql", totalStarted, err)
	if err != nil {
		return nil, err
	}
	return items, nil
}
