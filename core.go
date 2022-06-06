package zapdriver

import (
	"regexp"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// driverConfig is used to configure core.
type driverConfig struct {
	// Report all logs with level error or above to stackdriver using
	// `ErrorReport()` when set to true
	ReportAllErrors bool

	// ServiceName is added as `ServiceContext()` to all logs when set
	ServiceName string

	// ServiceVersion is added as `ServiceVersionContext()` to all logs when set
	ServiceVersion string
}

// Core is a zapdriver specific core wrapped around the default zap core. It
// allows to merge all defined labels
type core struct {
	zapcore.Core

	// permLabels is a collection of labels that have been added to the logger
	// through the use of `With()`. These labels should never be cleared after
	// logging a single entry, unlike `tempLabel`.
	permLabels *labels

	// tempLabels keeps a record of all the labels that need to be applied to the
	// current log entry. Zap serializes log fields at different parts of the
	// stack, one such location is when calling `core.With` and the other one is
	// when calling `core.Write`. This makes it impossible to (for example) take
	// all `labels.xxx` fields, and wrap them in the `labels` namespace in one go.
	//
	// Instead, we have to filter out these labels at both locations, and then add
	// them back in the proper format right before we call `Write` on the original
	// Zap core.
	tempLabels *labels

	// Configuration for the zapdriver core
	config driverConfig
}

// zapdriver core option to report all logs with level error or above to stackdriver
// using `ErrorReport()` when set to true
func ReportAllErrors(report bool) func(*core) {
	return func(c *core) {
		c.config.ReportAllErrors = report
	}
}

// zapdriver core option to add `ServiceContext()` to all logs with `name` as
// service name
func ServiceName(name string) func(*core) {
	return func(c *core) {
		c.config.ServiceName = name
	}
}

// zapdriver core option to add `ServiceVersion()` to all logs with `version` as
// service version
func ServiceVersion(version string) func(*core) {
	return func(c *core) {
		c.config.ServiceVersion = version
	}
}

// WrapCore returns a `zap.Option` that wraps the default core with the
// zapdriver one.
func WrapCore(options ...func(*core)) zap.Option {
	return zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		newcore := &core{
			Core:       c,
			permLabels: newLabels(),
			tempLabels: newLabels(),
		}
		for _, option := range options {
			option(newcore)
		}
		return newcore
	})
}

// With adds structured context to the Core.
func (c *core) With(fields []zap.Field) zapcore.Core {
	var lbls *labels
	lbls, fields = c.extractLabels(fields)

	// copy permLabels
	permLabels := newLabels()
	c.permLabels.mutex.RLock()
	for k, v := range c.permLabels.store {
		permLabels.store[k] = v
	}
	c.permLabels.mutex.RUnlock()

	for k, v := range lbls.store {
		permLabels.store[k] = v
	}

	return &core{
		Core:       c.Core.With(fields),
		permLabels: permLabels,
		tempLabels: newLabels(),
		config:     c.config,
	}
}

// Check determines whether the supplied Entry should be logged (using the
// embedded LevelEnabler and possibly some extra logic). If the entry
// should be logged, the Core adds itself to the CheckedEntry and returns
// the result.
//
// Callers must use Check before calling Write.
func (c *core) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}

	return ce
}

// multiline pattern to match the function name line
var functionNamePattern = regexp.MustCompile(`(?m)^(\S+)$`)

func (c *core) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	var lbls *labels
	lbls, fields = c.extractLabels(fields)

	lbls.mutex.RLock()
	c.tempLabels.mutex.Lock()
	for k, v := range lbls.store {
		c.tempLabels.store[k] = v
	}
	c.tempLabels.mutex.Unlock()
	lbls.mutex.RUnlock()

	fields = mergeLabelFields(fields, c.allLabels())
	fields = c.withSourceLocation(ent, fields)
	if c.config.ServiceName != "" {
		fields = c.withServiceContext(c.config.ServiceName, c.config.ServiceVersion, fields)
	}
	if c.config.ReportAllErrors && zapcore.ErrorLevel.Enabled(ent.Level) {
		fields = c.withErrorReport(ent, fields)
		if c.config.ServiceName == "" {
			// A service name was not set but error report needs it
			// So attempt to add a generic service name
			fields = c.withServiceContext("unknown", c.config.ServiceVersion, fields)
		}
	}

	c.tempLabels.reset()

	// https://github.com/evanj/gcplogs/blob/4d593c6e8beb16c134af8baff3d266d73573d16f/gcpzap/encoder.go#L46
	if ent.Stack != "" {
		// Make the message look like a real panic, so Stackdriver error reporting picks it up.
		// This used to need the string "panic: " at the beginning, but no longer seems to need it!
		// ent.Message = "panic: " + ent.Message + "\n\ngoroutine 1 [running]:\n"
		ent.Message = ent.Message + "\n\ngoroutine 1 [running]:\n"
		// Trial-and-error: On App Engine Standard go111 the () are needed after function calls
		// zap does not add them, so hack it with a regexp
		replaced := functionNamePattern.ReplaceAllString(ent.Stack, "$1(...)")
		ent.Message += replaced
		ent.Stack = ""
	}

	return c.Core.Write(ent, fields)
}

// Sync flushes buffered logs (if any).
func (c *core) Sync() error {
	return c.Core.Sync()
}

func (c *core) allLabels() *labels {
	lbls := newLabels()

	lbls.mutex.Lock()
	c.permLabels.mutex.RLock()
	for k, v := range c.permLabels.store {
		lbls.store[k] = v
	}
	c.permLabels.mutex.RUnlock()

	c.tempLabels.mutex.RLock()
	for k, v := range c.tempLabels.store {
		lbls.store[k] = v
	}
	c.tempLabels.mutex.RUnlock()
	lbls.mutex.Unlock()

	return lbls
}

func (c *core) extractLabels(fields []zapcore.Field) (*labels, []zapcore.Field) {
	lbls := newLabels()
	out := []zapcore.Field{}

	lbls.mutex.Lock()
	for i := range fields {
		if !isLabelField(fields[i]) {
			out = append(out, fields[i])
			continue
		}

		lbls.store[strings.Replace(fields[i].Key, "labels.", "", 1)] = fields[i].String
	}
	lbls.mutex.Unlock()

	return lbls, out
}

func (c *core) withLabels(fields []zapcore.Field) []zapcore.Field {
	lbls := newLabels()
	out := []zapcore.Field{}

	lbls.mutex.Lock()
	for i := range fields {
		if isLabelField(fields[i]) {
			lbls.store[strings.Replace(fields[i].Key, "labels.", "", 1)] = fields[i].String
			continue
		}

		out = append(out, fields[i])
	}
	lbls.mutex.Unlock()

	return append(out, labelsField(lbls))
}

func (c *core) withSourceLocation(ent zapcore.Entry, fields []zapcore.Field) []zapcore.Field {
	// If the source location was manually set, don't overwrite it
	for i := range fields {
		if fields[i].Key == sourceKey {
			return fields
		}
	}

	if !ent.Caller.Defined {
		return fields
	}

	return append(fields, SourceLocation(ent.Caller.PC, ent.Caller.File, ent.Caller.Line, true))
}

func (c *core) withServiceContext(name, version string, fields []zapcore.Field) []zapcore.Field {
	// If the service context was manually set, don't overwrite it
	for i := range fields {
		if fields[i].Key == serviceContextKey {
			return fields
		}
	}

	return append(fields, ServiceContext(name, version))
}

func (c *core) withErrorReport(ent zapcore.Entry, fields []zapcore.Field) []zapcore.Field {
	// If the error report was manually set, don't overwrite it
	for i := range fields {
		if fields[i].Key == contextKey {
			return fields
		}
	}

	if !ent.Caller.Defined {
		return fields
	}

	return append(fields, ErrorReport(ent.Caller.PC, ent.Caller.File, ent.Caller.Line, true))
}
