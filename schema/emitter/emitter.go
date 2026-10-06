package emitter

import (
	"context"
	"time"

	"github.com/coroot/coroot-cluster-agent/common"
	"github.com/coroot/coroot-cluster-agent/schema"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
)

type ChangeEmitter struct {
	logger log.Logger
}

func NewChangeEmitter() (*ChangeEmitter, error) {
	provider, err := common.NewLoggerProvider("DatabaseChanges")
	if err != nil {
		return nil, err
	}
	return &ChangeEmitter{logger: provider.Logger("coroot-cluster-agent")}, nil
}

func (e *ChangeEmitter) Emit(change schema.Change, dbSystem, targetAddr string) {
	record := log.Record{}
	record.SetTimestamp(time.Now())
	record.SetSeverity(log.SeverityInfo)
	record.SetSeverityText("Info")
	record.SetBody(attribute.StringValue(change.Diff))
	record.AddAttributes(
		attribute.String("db.system", dbSystem),
		attribute.String("db.target", targetAddr),
		attribute.String("db.name", change.Database),
		attribute.String("db_change.object", change.Object),
		attribute.String("db_change.type", change.Type),
	)
	e.logger.Emit(context.TODO(), record)
}
