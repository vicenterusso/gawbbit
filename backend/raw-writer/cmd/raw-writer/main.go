// Command raw-writer consumes events from NATS JetStream and lands them as Snappy
// Parquet files in GCS or an S3-compatible object store.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/logx"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/natsconsumer"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/service"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/settings"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/storage"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "raw-writer: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	s, err := settings.Load()
	if err != nil {
		return err
	}
	level, err := logx.ParseLevel(s.LogLevel)
	if err != nil {
		return err
	}
	logx.Std.SetLevel(level)

	uploader, err := storage.New(context.Background(), s)
	if err != nil {
		return err
	}
	svc, err := service.New(s, natsconsumer.New(s), uploader)
	if err != nil {
		return err
	}
	release := service.HandleSignals(svc)
	defer release()

	runErr := svc.Run()
	if closeErr := svc.Close(); runErr == nil {
		runErr = closeErr
	}
	return runErr
}
