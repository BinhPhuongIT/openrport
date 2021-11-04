//+build windows

package system

import (
	"context"
	"strings"
)

func (s *realSystemInfo) virtualizationInfo(ctx context.Context) (virtSystem, virtRole string, err error) {
	execCtx := &CmdExecutorContext{
		Interpreter: "powerShell",
		Command:     "Get-Service",
	}
	cmd := s.cmdExec.New(ctx, execCtx)
	execRes, err := cmd.CombinedOutput()

	if err != nil {
		return "", "", err
	}

	sysInfo := strings.TrimSpace(string(execRes))

	virtSystem, virtRole = getVirtInfoFromPowershellServicesList(sysInfo)

	return virtSystem, virtRole, nil
}
