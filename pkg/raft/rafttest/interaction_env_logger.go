// This code has been modified from its original form by The Cockroach Authors.
// All modifications are Copyright 2024 The Cockroach Authors.
//
// Copyright 2019 The etcd Authors
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

package rafttest

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/raft"
)

type logLevels [6]string

var lvlNames logLevels = [...]string{"DEBUG", "INFO", "WARN", "ERROR", "FATAL", "NONE"}

type RedirectLogger struct {
	*strings.Builder
	Lvl int // 0 = DEBUG, 1 = INFO, 2 = WARNING, 3 = ERROR, 4 = FATAL, 5 = NONE
}

var _ raft.Logger = (*RedirectLogger)(nil)

func (l *RedirectLogger) printf(lvl int, format string, args ...interface{}) {
	if l.Lvl <= lvl {
		fmt.Fprint(l, lvlNames[lvl], " ")
		fmt.Fprintf(l, format, args...)
		if n := len(format); n > 0 && format[n-1] != '\n' {
			l.WriteByte('\n')
		}
	}
}
func (l *RedirectLogger) print(lvl int, args ...interface{}) {
	if l.Lvl <= lvl {
		fmt.Fprint(l, lvlNames[lvl], " ")
		fmt.Fprintln(l, args...)
	}
}

func (l *RedirectLogger) Debug(v ...interface{}) {
	l.print(0, v...)
}

func (l *RedirectLogger) Debugf(format string, v ...interface{}) {
	l.printf(0, format, v...)
}

func (l *RedirectLogger) Info(v ...interface{}) {
	l.print(1, v...)
}

func (l *RedirectLogger) Infof(format string, v ...interface{}) {
	l.printf(1, format, v...)
}

func (l *RedirectLogger) Warning(v ...interface{}) {
	l.print(2, v...)
}

func (l *RedirectLogger) Warningf(format string, v ...interface{}) {
	l.printf(2, format, v...)
}

func (l *RedirectLogger) Error(v ...interface{}) {
	l.print(3, v...)
}

func (l *RedirectLogger) Errorf(format string, v ...interface{}) {
	l.printf(3, format, v...)
}

func (l *RedirectLogger) Fatal(v ...interface{}) {
	l.print(4, v...)
	panic(fmt.Sprint(v...))
}

func (l *RedirectLogger) Fatalf(format string, v ...interface{}) {
	l.printf(4, format, v...)
	panic(fmt.Sprintf(format, v...))
}

func (l *RedirectLogger) Panic(v ...interface{}) {
	l.print(4, v...)
	panic(fmt.Sprint(v...))
}

func (l *RedirectLogger) Panicf(format string, v ...interface{}) {
	l.printf(4, format, v...)
	// TODO(pavelkalinnikov): catch the panic gracefully in datadriven package.
	// This would allow observing all the intermediate logging while debugging,
	// and testing the cases when panic is expected.
	panic(fmt.Sprintf(format, v...))
}

// Override StringBuilder write methods to silence them under NONE.

func (l *RedirectLogger) Quiet() bool {
	return l.Lvl == len(lvlNames)-1
}

func (l *RedirectLogger) Write(p []byte) (int, error) {
	if l.Quiet() {
		return 0, nil
	}
	return l.Builder.Write(p)
}

func (l *RedirectLogger) WriteByte(c byte) error {
	if l.Quiet() {
		return nil
	}
	return l.Builder.WriteByte(c)
}

func (l *RedirectLogger) WriteRune(r rune) (int, error) {
	if l.Quiet() {
		return 0, nil
	}
	return l.Builder.WriteRune(r)
}

func (l *RedirectLogger) WriteString(s string) (int, error) {
	if l.Quiet() {
		return 0, nil
	}
	return l.Builder.WriteString(s)
}
