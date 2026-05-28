//go:build !windows

package ptyrun

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKillProcessGroupNil(t *testing.T) {
	var p *Process

	assert.NoError(t, p.killProcessGroup())
}
