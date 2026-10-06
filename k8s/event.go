package k8s

import (
	"context"
	"time"

	"github.com/coroot/coroot-cluster-agent/common"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	corev1 "k8s.io/api/core/v1"
)

type EventsLogger struct {
	logger log.Logger
}

func NewEventsLogger() (*EventsLogger, error) {
	provider, err := common.NewLoggerProvider("KubernetesEvents")
	if err != nil {
		return nil, err
	}
	return &EventsLogger{logger: provider.Logger("coroot-cluster-agent")}, nil
}

func (l *EventsLogger) EmitEvent(event *corev1.Event) {
	record := log.Record{}
	ts := event.LastTimestamp.Time
	if ts.IsZero() {
		ts = event.EventTime.Time
	}
	if ts.IsZero() {
		ts = time.Now()
	}
	record.SetTimestamp(ts)
	record.SetSeverityText(event.Type)
	switch event.Type {
	case corev1.EventTypeNormal:
		record.SetSeverity(log.SeverityInfo)
	case corev1.EventTypeWarning:
		record.SetSeverity(log.SeverityWarn)
	}
	record.SetBody(attribute.StringValue(event.Message))
	record.AddAttributes(
		attribute.String("event.name", event.Name),
		attribute.String("event.namespace", event.Namespace),
		attribute.String("event.reason", event.Reason),
		attribute.String("object.kind", event.InvolvedObject.Kind),
		attribute.String("object.name", event.InvolvedObject.Name),
		attribute.String("object.namespace", event.InvolvedObject.Namespace),
		attribute.String("source.component", event.Source.Component),
		attribute.String("source.host", event.Source.Host),
	)
	l.logger.Emit(context.TODO(), record)
}
