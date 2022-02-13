package chclient

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudradar-monitoring/rport/client/system"
	"github.com/cloudradar-monitoring/rport/share/clientconfig"
	"github.com/cloudradar-monitoring/rport/share/comm"
	"github.com/cloudradar-monitoring/rport/share/logger"
	"github.com/cloudradar-monitoring/rport/share/test"
)

type CmdExecutorMock struct {
	DoneChannel    chan bool
	ReturnPID      int
	ReturnStartErr error
	ReturnWaitErr  error
	ReturnStdOut   []string
	ReturnStdErr   []string

	wg sync.WaitGroup
}

func NewCmdExecutorMock() *CmdExecutorMock {
	return &CmdExecutorMock{}
}

func (e *CmdExecutorMock) New(ctx context.Context, execCtx *system.CmdExecutorContext) *exec.Cmd {
	var args []string
	if execCtx.IsSudo {
		args = append(args, "sudo -n")
	}

	args = append(args, execCtx.Command)

	cmd := exec.CommandContext(ctx, execCtx.Interpreter.InterpreterNameFromInput, args...)
	cmd.Dir = execCtx.WorkingDir
	return cmd
}

func (e *CmdExecutorMock) Start(cmd *exec.Cmd) error {
	if e.ReturnStartErr != nil {
		return e.ReturnStartErr
	}

	if e.ReturnPID != 0 {
		cmd.Process = &os.Process{Pid: e.ReturnPID}
	}

	// mock output to stdout and stderr
	e.wg.Add(1)
	go e.writeToStdOut(cmd)
	e.wg.Add(1)
	go e.writeToStdErr(cmd)

	return nil
}

func (e *CmdExecutorMock) writeToStdOut(cmd *exec.Cmd) {
	defer e.wg.Done()

	for _, s := range e.ReturnStdOut {
		_, err := cmd.Stdout.Write([]byte(s))
		if err != nil {
			log.Printf("Failed to write data into stdout: %s", err)
			return
		}
	}
}

func (e *CmdExecutorMock) writeToStdErr(cmd *exec.Cmd) {
	defer e.wg.Done()

	for _, s := range e.ReturnStdErr {
		_, err := cmd.Stderr.Write([]byte(s))
		if err != nil {
			log.Printf("Failed to write data into stderr: %s", err)
			return
		}
	}
}

func (e *CmdExecutorMock) Wait(cmd *exec.Cmd) error {
	if e.ReturnWaitErr != nil {
		return e.ReturnWaitErr
	}
	e.wg.Wait()
	// wait if needed
	if e.DoneChannel != nil {
		e.DoneChannel <- true
	}
	return nil
}

// nowMock is used to override time now.
var nowMockF = func() time.Time {
	n, _ := time.Parse(time.RFC3339, "2020-08-19T12:00:00+03:00")
	return n
}

var nowMock = nowMockF()

var testLog = logger.NewLogger("client", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)

const jobToRunJSON = `
{
	"jid": "5f02b216-3f8a-42be-b66c-f4c1d0ea3809",
	"client_id": "d81e6b93e75aef59a7701b90555f43808458b34e30370c3b808c1816a32252b3",
	"command": "/bin/date;foo;whoami",
	"created_by": "admin",
	"timeout_sec": 60,
	"is_sudo": true,
	"cwd": "/root"
}
`
const scriptToRunJSON = `
{
	"jid": "5f02b216-3f8a-42be-b66c-f4c1d0ea3810",
	"client_id": "d81e6b93e75aef59a7701b90555f43808458b34e30370c3b808c1816a32252b5",
	"command": "pwd",
	"is_script": true
}
`

func TestHandleRunCmdRequestPositiveCase(t *testing.T) {
	now = nowMockF

	wantPID := 123
	execMock := NewCmdExecutorMock()
	execMock.ReturnPID = wantPID
	execMock.ReturnStdOut = []string{"output1", "output2", "output3"}
	execMock.ReturnStdErr = []string{"error1", "error2"}
	connMock := test.NewConnMock()
	// mimic real behavior and wait until background task sends the request
	done := make(chan bool)
	connMock.DoneChannel = done
	configCopy := getDefaultValidMinConfig()
	c := Client{
		cmdExec:      execMock,
		sshConn:      connMock,
		Logger:       testLog,
		configHolder: &configCopy,
	}

	configCopy.Client.DataDir = filepath.Join(configCopy.Client.DataDir, "TestHandleRunCmdRequestPositiveCase")
	defer func() {
		os.RemoveAll(configCopy.Client.DataDir)
	}()
	err := PrepareDirs(&configCopy)
	require.NoError(t, err)

	wantJSONPart1 := `
{
	"jid": "5f02b216-3f8a-42be-b66c-f4c1d0ea3809",
	"status": "successful",
	"is_sudo": true,
	"is_script": false,
	"finished_at": "2020-08-19T12:00:00+03:00",
	"client_id": "d81e6b93e75aef59a7701b90555f43808458b34e30370c3b808c1816a32252b3",
	"client_name": "",
	"command": "/bin/date;foo;whoami",
	"interpreter": "",
	"pid": 123,
	"started_at": "2020-08-19T12:00:00+03:00",
	"created_by": "admin",
	"cwd": "/root",
	"timeout_sec": 60,
	"multi_job_id":null,
	"schedule_id":null,
	"error":"%s",
`
	wantJSONPart2 := `
	  "result": {
			"stdout": "output1output2output3",
			"stderr": "error1error2"
		}
	}
	`
	stdOutSize := len(strings.Join(execMock.ReturnStdOut, ""))
	stdErrSize := len(strings.Join(execMock.ReturnStdErr, ""))

	testCases := []struct {
		name            string
		sendBackLimit   int
		denyRegexp      *regexp.Regexp
		wantJSON        string
		wantErrContains string
	}{
		{
			name:          "limit is larger than stdout and stderr",
			sendBackLimit: stdOutSize + 1,
			wantJSON:      fmt.Sprintf(wantJSONPart1, "") + wantJSONPart2,
		},
		{
			name:          "limit is equal to the larger output",
			sendBackLimit: stdOutSize,
			wantJSON:      fmt.Sprintf(wantJSONPart1, "") + wantJSONPart2,
		},
		{
			name:          "limit is equal to the smaller output",
			sendBackLimit: stdErrSize,
			wantJSON: fmt.Sprintf(wantJSONPart1, "overflow of stdOut buffer: maximum send_back_limit of 12 bytes exceeded") + `
       "result": {
       "stdout": "output1outpu",
       "stderr": "error1error2"
   }
}`,
		},
		{
			name:          "limit is less than smaller output",
			sendBackLimit: stdErrSize - 1,
			wantJSON: fmt.Sprintf(wantJSONPart1, "overflow of stdOut buffer: maximum send_back_limit of 11 bytes exceeded, overflow of stdErr buffer: maximum send_back_limit of 11 bytes exceeded") + `
		"result": {
		"stdout": "output1outp",
		"stderr": "error1error"
	}
}`,
		},
		{
			name:          "limit is zero",
			sendBackLimit: 0,
			wantJSON: fmt.Sprintf(wantJSONPart1, "overflow of stdOut buffer: maximum send_back_limit of 0 bytes exceeded, overflow of stdErr buffer: maximum send_back_limit of 0 bytes exceeded") + `
				"result": {
				"stdout": "",
				"stderr": ""
			}
		}`,
		},
		{
			name:            "command is not allowed",
			denyRegexp:      regexp.MustCompile(".*"),
			wantErrContains: "command is not allowed",
		},
	}
	for i := range testCases {
		tc := testCases[i]
		t.Run(tc.name, func(t *testing.T) {
			// given
			c.configHolder.RemoteCommands.SendBackLimit = tc.sendBackLimit
			if tc.denyRegexp != nil {
				c.configHolder.RemoteCommands.DenyRegexp = []*regexp.Regexp{tc.denyRegexp}
			}

			// when
			res, err := c.HandleRunCmdRequest(context.Background(), []byte(jobToRunJSON))

			// then
			if tc.wantErrContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrContains)
				return
			}
			require.NoError(t, err)
			<-done

			// check returned result
			assert.Equal(t, &comm.RunCmdResponse{Pid: wantPID, StartedAt: nowMock}, res)

			// check job result that was sent to server
			inputRequestName, inputWantReply, inputPayload := connMock.InputSendRequest()
			assert.Equal(t, comm.RequestTypeCmdResult, inputRequestName)
			assert.Equal(t, false, inputWantReply)
			assert.JSONEq(t, tc.wantJSON, string(inputPayload))
		})
	}
}

func TestRemoteCommandsDisabled(t *testing.T) {
	// given
	c := Client{
		Logger: testLog,
		configHolder: &ClientConfigHolder{
			Config: &clientconfig.Config{
				RemoteCommands: clientconfig.CommandsConfig{
					Enabled: false,
				},
				RemoteScripts: clientconfig.ScriptsConfig{
					Enabled: true,
				},
			},
		},
	}

	// when
	gotRes, gotErr := c.HandleRunCmdRequest(context.Background(), []byte(jobToRunJSON))

	// then
	require.Error(t, gotErr)
	assert.Equal(t, "remote commands execution is disabled", gotErr.Error())
	assert.Nil(t, gotRes)
}

func TestRemoteScriptsDisabled(t *testing.T) {
	c := Client{
		Logger: testLog,
		configHolder: &ClientConfigHolder{
			Config: &clientconfig.Config{
				RemoteCommands: clientconfig.CommandsConfig{
					Enabled: true,
				},
				RemoteScripts: clientconfig.ScriptsConfig{
					Enabled: false,
				},
			},
		},
	}

	_, gotErr := c.HandleRunCmdRequest(context.Background(), []byte(scriptToRunJSON))

	require.EqualError(t, gotErr, "remote scripts are disabled")
}

func TestIsCommandAllowed(t *testing.T) {
	defaultTestAllow := []string{"^/usr/bin.*", "^/usr/local/bin/.*", `^C:\\Windows\\System32.*`}
	testCases := []struct {
		name string

		cmd   string
		order [2]string
		allow []string
		deny  []string

		wantRes bool
	}{
		{
			name:    "allow-deny: does not match allow regexp",
			cmd:     "/some/cmd",
			order:   allowDenyOrder,
			allow:   defaultTestAllow,
			wantRes: false,
		},
		{
			name:    "allow-deny: matches both allow and deny regexp",
			cmd:     "/usr/bin/zip",
			order:   allowDenyOrder,
			allow:   defaultTestAllow,
			deny:    []string{"^/usr/bin/z.*"},
			wantRes: false,
		},
		{
			name:    "allow-deny: matches allow, empty deny",
			cmd:     "/usr/bin/zip",
			order:   allowDenyOrder,
			allow:   defaultTestAllow,
			deny:    []string{},
			wantRes: true,
		},
		{
			name:    "windows: allow-deny: matches allow, empty deny",
			cmd:     `C:\Windows\System32\some`,
			order:   allowDenyOrder,
			allow:   defaultTestAllow,
			deny:    []string{},
			wantRes: true,
		},
		{
			name:    "allow-deny: empty allow, does not match deny regexp",
			cmd:     "/bin/some/cmd",
			order:   allowDenyOrder,
			allow:   []string{},
			deny:    []string{"^/usr/bin/zip.*"},
			wantRes: false,
		},
		{
			name:    "allow-deny: matches allow regexp but not deny",
			cmd:     "/usr/bin/zip",
			order:   allowDenyOrder,
			allow:   defaultTestAllow,
			deny:    []string{"^/usr/bin/zip2.*", "zip3.*"},
			wantRes: true,
		},
		{
			name:    "allow-deny: does not match any regexp",
			cmd:     "/bin/some/cmd",
			order:   allowDenyOrder,
			allow:   defaultTestAllow,
			deny:    []string{"^/usr/bin/zip2.*"},
			wantRes: false,
		},
		{
			name:    "deny-allow: matches both deny and allow regexp",
			cmd:     "/usr/bin/zip",
			order:   denyAllowOrder,
			deny:    []string{"^/usr/bin/z.*"},
			allow:   defaultTestAllow,
			wantRes: true,
		},
		{
			name:    "deny-allow: matches deny regexp but not allow",
			cmd:     "/usr/test/test-cmd",
			order:   denyAllowOrder,
			deny:    []string{".*test.*"},
			allow:   []string{"^/usr/bin.*", "^/usr/local/bin/.*"},
			wantRes: false,
		},
		{
			name:    "deny-allow: matches allow regexp but not deny",
			cmd:     "/usr/bin/zip",
			order:   denyAllowOrder,
			deny:    []string{"^/usr/bin/zip2.*", ".*zip3.*"},
			allow:   defaultTestAllow,
			wantRes: true,
		},
		{
			name:    "allow-deny: does not match any regexp",
			cmd:     "/bin/some/cmd",
			order:   denyAllowOrder,
			allow:   defaultTestAllow,
			deny:    []string{"^/usr/bin/zip2.*"},
			wantRes: true,
		},
		{
			name:    "unknown order",
			cmd:     "/bin/some/cmd",
			order:   [2]string{"one", "two"},
			allow:   defaultTestAllow,
			deny:    []string{"^/usr/bin/zip.*"},
			wantRes: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// given
			config := getDefaultValidMinConfig()
			config.RemoteCommands.Deny = tc.deny
			c := Client{
				Logger:       testLog,
				configHolder: &config,
			}
			c.configHolder.RemoteCommands.Order = tc.order
			c.configHolder.RemoteCommands.AllowRegexp = getRegexpList(tc.allow)
			c.configHolder.RemoteCommands.DenyRegexp = getRegexpList(tc.deny)

			// when
			gotRes := c.isAllowed(tc.cmd)

			// then
			assert.Equal(t, tc.wantRes, gotRes)
		})
	}
}

func getRegexpList(list []string) []*regexp.Regexp {
	var res []*regexp.Regexp
	for _, v := range list {
		res = append(res, regexp.MustCompile(v))
	}
	return res
}
