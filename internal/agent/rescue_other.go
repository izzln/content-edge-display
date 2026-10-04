//go:build !linux

package agent

import "context"

// 非 Linux（开发机）没有 /dev/input 与 tty1：不提供现场救援。
func watchKeyboards(ctx context.Context) <-chan struct{} { return nil }

type vtConsole struct{}

func (vtConsole) show(string) {}
func (vtConsole) hide()       {}
func (vtConsole) close()      {}
