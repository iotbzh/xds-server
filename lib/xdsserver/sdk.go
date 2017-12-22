/*
 * Copyright (C) 2017 "IoT.bzh"
 * Author Sebastien Douheret <sebastien@iot.bzh>
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package xdsserver

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	common "github.com/iotbzh/xds-common/golib"
	"github.com/iotbzh/xds-common/golib/eows"
	"github.com/iotbzh/xds-server/lib/xsapiv1"
	uuid "github.com/satori/go.uuid"
)

// Definition of scripts used to managed SDKs
const (
	scriptAdd       = "add"
	scriptGetConfig = "get-config"
	scriptList      = "list"
	scriptRemove    = "remove"
	scriptUpdate    = "update"
)

var scriptsAll = []string{
	scriptAdd,
	scriptGetConfig,
	scriptList,
	scriptRemove,
	scriptUpdate,
}

var sdkCmdID = 0

// CrossSDK Hold SDK config
type CrossSDK struct {
	*Context
	sdk        xsapiv1.SDK
	scripts    map[string]string
	installCmd *eows.ExecOverWS
	removeCmd  *eows.ExecOverWS

	bufStdout string
	bufStderr string
}

// ListCrossSDK List all available and installed SDK  (call "list" script)
func ListCrossSDK(scriptDir string, log *logrus.Logger) ([]xsapiv1.SDK, error) {
	sdksList := []xsapiv1.SDK{}

	// Retrieve SDKs list and info
	cmd := exec.Command(path.Join(scriptDir, scriptList))
	stdout, err := cmd.CombinedOutput()
	if err != nil {
		return sdksList, fmt.Errorf("Cannot get sdks list: %v", err)
	}

	if err = json.Unmarshal(stdout, &sdksList); err != nil {
		log.Errorf("SDK list script output:\n%v\n", string(stdout))
		return sdksList, fmt.Errorf("Cannot decode sdk list %v", err)
	}

	return sdksList, nil
}

// NewCrossSDK creates a new instance of Syncthing
func NewCrossSDK(ctx *Context, sdk xsapiv1.SDK, scriptDir string) (*CrossSDK, error) {
	s := CrossSDK{
		Context: ctx,
		sdk:     sdk,
		scripts: make(map[string]string),
	}

	// Execute get-config script to retrieve SDK configuration
	getConfFile := path.Join(scriptDir, scriptGetConfig)
	if !common.Exists(getConfFile) {
		return &s, fmt.Errorf("'%s' script file not found in %s", scriptGetConfig, scriptDir)
	}

	cmd := exec.Command(getConfFile)
	stdout, err := cmd.CombinedOutput()
	if err != nil {
		return &s, fmt.Errorf("Cannot get sdk config using %s: %v", getConfFile, err)
	}

	err = json.Unmarshal(stdout, &s.sdk.FamilyConf)
	if err != nil {
		s.Log.Errorf("SDK config script output:\n%v\n", string(stdout))
		return &s, fmt.Errorf("Cannot decode sdk config %v", err)
	}
	famName := s.sdk.FamilyConf.FamilyName

	// Sanity check
	if s.sdk.FamilyConf.RootDir == "" {
		return &s, fmt.Errorf("SDK config not valid (rootDir not set)")
	}
	if s.sdk.FamilyConf.EnvSetupFile == "" {
		return &s, fmt.Errorf("SDK config not valid (envSetupFile not set)")
	}

	// Check that other mandatory scripts are present
	for _, scr := range scriptsAll {
		s.scripts[scr] = path.Join(scriptDir, scr)
		if !common.Exists(s.scripts[scr]) {
			return &s, fmt.Errorf("Script named '%s' missing in SDK family '%s'", scr, famName)
		}
	}

	// Fixed default fields value
	sdk.LastError = ""
	if sdk.Status == "" {
		sdk.Status = xsapiv1.SdkStatusNotInstalled
	}

	// Sanity check
	errMsg := "Invalid SDK definition "
	if sdk.Name == "" {
		return &s, fmt.Errorf(errMsg + "(name not set)")
	} else if sdk.Profile == "" {
		return &s, fmt.Errorf(errMsg + "(profile not set)")
	} else if sdk.Version == "" {
		return &s, fmt.Errorf(errMsg + "(version not set)")
	} else if sdk.Arch == "" {
		return &s, fmt.Errorf(errMsg + "(arch not set)")
	}
	if sdk.Status == xsapiv1.SdkStatusInstalled {
		if sdk.SetupFile == "" {
			return &s, fmt.Errorf(errMsg + "(setupFile not set)")
		} else if !common.Exists(sdk.SetupFile) {
			return &s, fmt.Errorf(errMsg + "(setupFile not accessible)")
		}
		if sdk.Path == "" {
			return &s, fmt.Errorf(errMsg + "(path not set)")
		} else if !common.Exists(sdk.Path) {
			return &s, fmt.Errorf(errMsg + "(path not accessible)")
		}
	}

	// Use V3 to ensure that we get same uuid on restart
	nm := s.sdk.Name
	if nm == "" {
		nm = s.sdk.Profile + "_" + s.sdk.Arch + "_" + s.sdk.Version
	}
	s.sdk.ID = uuid.NewV3(uuid.FromStringOrNil("sdks"), nm).String()

	s.LogSillyf("New SDK: ID=%v, Family=%s, Name=%v", s.sdk.ID[:8], s.sdk.FamilyConf.FamilyName, s.sdk.Name)

	return &s, nil
}

// Install a SDK (non blocking command, IOW run in background)
func (s *CrossSDK) Install(file string, force bool, timeout int, sess *ClientSession) error {

	if s.sdk.Status == xsapiv1.SdkStatusInstalled {
		return fmt.Errorf("already installed")
	}
	if s.sdk.Status == xsapiv1.SdkStatusInstalling {
		return fmt.Errorf("installation in progress")
	}

	// Compute command args
	cmdArgs := []string{}
	if file != "" {
		cmdArgs = append(cmdArgs, "--file", file)
	} else {
		cmdArgs = append(cmdArgs, "--url", s.sdk.URL)
	}
	if force {
		cmdArgs = append(cmdArgs, "--force")
	}

	// Unique command id
	sdkCmdID++
	cmdID := "sdk-install-" + strconv.Itoa(sdkCmdID)

	// Create new instance to execute command and sent output over WS
	s.installCmd = eows.New(s.scripts[scriptAdd], cmdArgs, sess.IOSocket, sess.ID, cmdID)
	s.installCmd.Log = s.Log
	if timeout > 0 {
		s.installCmd.CmdExecTimeout = timeout
	} else {
		s.installCmd.CmdExecTimeout = 30 * 60 // default 30min
	}

	// FIXME: temporary hack
	s.bufStdout = ""
	s.bufStderr = ""
	SizeBufStdout := 10
	SizeBufStderr := 2000
	if valS, ok := os.LookupEnv("XDS_SDK_BUF_STDOUT"); ok {
		if valI, err := strconv.Atoi(valS); err == nil {
			SizeBufStdout = valI
		}
	}
	if valS, ok := os.LookupEnv("XDS_SDK_BUF_STDERR"); ok {
		if valI, err := strconv.Atoi(valS); err == nil {
			SizeBufStderr = valI
		}
	}

	// Define callback for output (stdout+stderr)
	s.installCmd.OutputCB = func(e *eows.ExecOverWS, stdout, stderr string) {
		// paranoia
		data := e.UserData
		sdkID := (*data)["SDKID"].(string)
		if sdkID != s.sdk.ID {
			s.Log.Errorln("BUG: sdk ID differs: %v != %v", sdkID, s.sdk.ID)
		}

		// IO socket can be nil when disconnected
		so := s.sessions.IOSocketGet(e.Sid)
		if so == nil {
			s.Log.Infof("%s not emitted: WS closed (sid:%s, msgid:%s)", xsapiv1.EVTSDKInstall, e.Sid, e.CmdID)
			return
		}

		if s.LogLevelSilly {
			s.Log.Debugf("%s emitted - WS sid[4:] %s - id:%s - SDK ID:%s:", xsapiv1.EVTSDKInstall, e.Sid[4:], e.CmdID, sdkID[:16])
			if stdout != "" {
				s.Log.Debugf("STDOUT <<%v>>", strings.Replace(stdout, "\n", "\\n", -1))
			}
			if stderr != "" {
				s.Log.Debugf("STDERR <<%v>>", strings.Replace(stderr, "\n", "\\n", -1))
			}
		}

		// Temporary "Hack": Buffered sent data to avoid freeze in web Browser
		// FIXME: remove bufStdout & bufStderr and implement better algorithm
		s.bufStdout += stdout
		s.bufStderr += stderr
		if len(s.bufStdout) > SizeBufStdout || len(s.bufStderr) > SizeBufStderr {
			// Emit event
			err := (*so).Emit(xsapiv1.EVTSDKInstall, xsapiv1.SDKManagementMsg{
				CmdID:     e.CmdID,
				Timestamp: time.Now().String(),
				Sdk:       s.sdk,
				Progress:  0, // TODO add progress
				Exited:    false,
				Stdout:    s.bufStdout,
				Stderr:    s.bufStderr,
			})
			if err != nil {
				s.Log.Errorf("WS Emit : %v", err)
			}
			s.bufStdout = ""
			s.bufStderr = ""
		}
	}

	// Define callback for output
	s.installCmd.ExitCB = func(e *eows.ExecOverWS, code int, exitError error) {
		// paranoia
		data := e.UserData
		sdkID := (*data)["SDKID"].(string)
		if sdkID != s.sdk.ID {
			s.Log.Errorln("BUG: sdk ID differs: %v != %v", sdkID, s.sdk.ID)
		}

		s.Log.Debugf("Command SDK ID %s [Cmd ID %s]  exited: code %d, exitError: %v", sdkID[:16], e.CmdID, code, exitError)

		// IO socket can be nil when disconnected
		so := s.sessions.IOSocketGet(e.Sid)
		if so == nil {
			s.Log.Infof("%s (exit) not emitted - WS closed (id:%s)", xsapiv1.EVTSDKInstall, e.CmdID)
			return
		}

		// Emit event remaining data in bufStdout/err
		if len(s.bufStderr) > 0 || len(s.bufStdout) > 0 {
			err := (*so).Emit(xsapiv1.EVTSDKInstall, xsapiv1.SDKManagementMsg{
				CmdID:     e.CmdID,
				Timestamp: time.Now().String(),
				Sdk:       s.sdk,
				Progress:  50, // TODO add progress
				Exited:    false,
				Stdout:    s.bufStdout,
				Stderr:    s.bufStderr,
			})
			if err != nil {
				s.Log.Errorf("WS Emit : %v", err)
			}
			s.bufStdout = ""
			s.bufStderr = ""
		}

		// Update SDK status
		if code == 0 && exitError == nil {
			s.sdk.LastError = ""
			s.sdk.Status = xsapiv1.SdkStatusInstalled
		} else {
			s.sdk.LastError = "Installation failed (code " + strconv.Itoa(code) +
				")"
			if exitError != nil {
				s.sdk.LastError = ". Error: " + exitError.Error()
			}
			s.sdk.Status = xsapiv1.SdkStatusNotInstalled
		}

		emitErr := ""
		if exitError != nil {
			emitErr = exitError.Error()
		}
		if emitErr == "" && s.sdk.LastError != "" {
			emitErr = s.sdk.LastError
		}

		// Emit event
		errSoEmit := (*so).Emit(xsapiv1.EVTSDKInstall, xsapiv1.SDKManagementMsg{
			CmdID:     e.CmdID,
			Timestamp: time.Now().String(),
			Sdk:       s.sdk,
			Progress:  100,
			Exited:    true,
			Code:      code,
			Error:     emitErr,
		})
		if errSoEmit != nil {
			s.Log.Errorf("WS Emit : %v", errSoEmit)
		}

		// Cleanup command for the next time
		s.installCmd = nil
	}

	// User data (used within callbacks)
	data := make(map[string]interface{})
	data["SDKID"] = s.sdk.ID
	s.installCmd.UserData = &data

	// Start command execution
	s.Log.Infof("Install SDK %s: cmdID=%v, cmd=%v, args=%v", s.sdk.Name, s.installCmd.CmdID, s.installCmd.Cmd, s.installCmd.Args)

	s.sdk.Status = xsapiv1.SdkStatusInstalling
	s.sdk.LastError = ""

	err := s.installCmd.Start()

	return err
}

// AbortInstallRemove abort an install or remove command
func (s *CrossSDK) AbortInstallRemove(timeout int) error {

	if s.installCmd == nil {
		return fmt.Errorf("no installation in progress for this sdk")
	}

	s.sdk.Status = xsapiv1.SdkStatusNotInstalled
	return s.installCmd.Signal("SIGKILL")
}

// Remove Used to remove/uninstall a SDK
func (s *CrossSDK) Remove() error {

	if s.sdk.Status != xsapiv1.SdkStatusInstalled {
		return fmt.Errorf("this sdk is not installed")
	}

	s.sdk.Status = xsapiv1.SdkStatusUninstalling

	cmdline := s.scripts[scriptRemove] + " " + s.sdk.Path
	cmd := exec.Command(cmdline)
	stdout, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Error while uninstalling sdk: %v", err)
	}
	s.Log.Debugf("SDK uninstall output:\n %v", stdout)

	return nil
}

// Get Return SDK definition
func (s *CrossSDK) Get() *xsapiv1.SDK {
	return &s.sdk
}

// GetEnvCmd returns the command used to initialized the environment
func (s *CrossSDK) GetEnvCmd() []string {
	return []string{"source", s.sdk.SetupFile}
}
