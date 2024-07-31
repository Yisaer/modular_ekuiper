// Copyright 2021-2023 EMQ Technologies Co., Ltd.
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

package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/lf-edge/ekuiper/internal/conf"
	"github.com/lf-edge/ekuiper/pkg/api"
	"github.com/lf-edge/ekuiper/pkg/infra"
)

var (
	once sync.Once
	pm   *pluginInsManager
)

// TODO setting configuration
var PortbleConf = &PortableConfig{
	SendTimeout: 1000,
}

// PluginIns created at two scenarios
// 1. At runtime, plugin is created/updated: in order to be able to reload rules that already uses previous ins
// 2. At system start/restart, when plugin is used by a rule
// Once created, never deleted until delete plugin command or system shutdown
type PluginIns struct {
	sync.RWMutex
	name     string
	ctrlChan ControlChannel // the same lifecycle as pluginIns, once created keep listening
	// audit the commands, so that when restarting the plugin, we can replay the commands
	commands map[Meta][]byte
	process  *os.Process // created when used by rule and deleted when no rule uses it
}

func NewPluginIns(name string, ctrlChan ControlChannel, process *os.Process) *PluginIns {
	return &PluginIns{
		process:  process,
		ctrlChan: ctrlChan,
		name:     name,
		commands: make(map[Meta][]byte),
	}
}

func NewPluginInsForTest(name string, ctrlChan ControlChannel) *PluginIns {
	commands := make(map[Meta][]byte)
	commands[Meta{
		RuleId:     "test",
		OpId:       "test",
		InstanceId: 0,
	}] = []byte{}
	return &PluginIns{
		process:  nil,
		ctrlChan: ctrlChan,
		name:     name,
		commands: commands,
	}
}

func (i *PluginIns) sendCmd(jsonArg []byte) error {
	err := i.ctrlChan.SendCmd(jsonArg)
	if err != nil && i.process == nil {
		return fmt.Errorf("plugin %s is not running sucessfully, please make sure it is valid", i.name)
	}
	return err
}

func (i *PluginIns) StartSymbol(ctx api.StreamContext, ctrl *Control) error {
	arg, err := json.Marshal(ctrl)
	if err != nil {
		return err
	}
	c := Command{
		Cmd: CMD_START,
		Arg: string(arg),
	}
	jsonArg, err := json.Marshal(c)
	if err != nil {
		return err
	}
	err = i.sendCmd(jsonArg)
	if err == nil {
		i.Lock()
		i.commands[ctrl.Meta] = jsonArg
		i.Unlock()
		ctx.GetLogger().Infof("started symbol %s", ctrl.SymbolName)
	}
	return err
}

func (i *PluginIns) StopSymbol(ctx api.StreamContext, ctrl *Control) error {
	arg, err := json.Marshal(ctrl)
	if err != nil {
		return err
	}
	c := Command{
		Cmd: CMD_STOP,
		Arg: string(arg),
	}
	jsonArg, err := json.Marshal(c)
	if err != nil {
		return err
	}
	err = i.sendCmd(jsonArg)
	if err == nil {
		i.Lock()
		delete(i.commands, ctrl.Meta)
		i.Unlock()
		ctx.GetLogger().Infof("stopped symbol %s", ctrl.SymbolName)
	}
	return err
}

// Stop intentionally
func (i *PluginIns) Stop() error {
	var err error
	i.RLock()
	defer i.RUnlock()
	if i.process != nil { // will also trigger process exit clean up
		err = i.process.Kill()
	}
	return err
}

// Manager plugin process and control socket
type pluginInsManager struct {
	instances map[string]*PluginIns
	sync.RWMutex
}

func GetPluginInsManager() *pluginInsManager {
	once.Do(func() {
		pm = &pluginInsManager{
			instances: make(map[string]*PluginIns),
		}
	})
	return pm
}

func (p *pluginInsManager) getPluginIns(name string) (*PluginIns, bool) {
	p.RLock()
	defer p.RUnlock()
	ins, ok := p.instances[name]
	return ins, ok
}

// DeletePluginIns should only run when there is no state aka. commands
func (p *pluginInsManager) DeletePluginIns(name string) {
	p.deletePluginIns(name)
}

// deletePluginIns should only run when there is no state aka. commands
func (p *pluginInsManager) deletePluginIns(name string) {
	p.Lock()
	defer p.Unlock()
	delete(p.instances, name)
}

// AddPluginIns For mock only
func (p *pluginInsManager) AddPluginIns(name string, ins *PluginIns) {
	p.Lock()
	defer p.Unlock()
	p.instances[name] = ins
}

// CreateIns Run when plugin is created/updated
func (p *pluginInsManager) CreateIns(pluginMeta *PluginMeta, isInit bool) error {
	p.Lock()
	defer p.Unlock()
	if isInit {
		go func() {
			_, err := p.getOrStartProcess(pluginMeta, PortbleConf)
			if err != nil {
				conf.Log.Errorf("create plugin %s failed: %v", pluginMeta.Name, err)
			} else {
				conf.Log.Infof("create plugin %s success", pluginMeta.Name)
			}
		}()
		return nil
	}
	_, err := p.getOrStartProcess(pluginMeta, PortbleConf)
	if err != nil {
		conf.Log.Errorf("create plugin %s failed: %v", pluginMeta.Name, err)
	} else {
		conf.Log.Infof("create plugin %s success", pluginMeta.Name)
	}
	return err
}

// getOrStartProcess Control the plugin process lifecycle.
// Need to manage the resources: instances map, control socket, plugin process
// May be called at plugin creation or restart with previous state(ctrlCh, commands)
// PluginIns is created by plugin manager but started by rule/funcop.
// During plugin delete/update, if the commands is not empty, keep the ins for next creation and restore
// 1. During creation, clean up those resources for any errors in defer immediately after the resource is created.
// 2. During plugin running, when detecting plugin process exit, clean up those resources for the current ins.
func (p *pluginInsManager) getOrStartProcess(pluginMeta *PluginMeta, pconf *PortableConfig) (_ *PluginIns, e error) {
	p.Lock()
	defer p.Unlock()
	var (
		ins *PluginIns
		ok  bool
	)
	// run initialization for firstly creating plugin instance
	ins, ok = p.instances[pluginMeta.Name]
	if !ok {
		ins = NewPluginIns(pluginMeta.Name, nil, nil)
		p.instances[pluginMeta.Name] = ins
	}
	// ins process has not run yet
	if ins.process != nil && ins.ctrlChan != nil {
		conf.Log.Infof("process %s alreayd started", pluginMeta.Name)
		return ins, nil
	}
	// should only happen for first start, then the ctrl channel will keep running
	if ins.ctrlChan == nil {
		ctrlChan, err := CreateControlChannel(pluginMeta.Name)
		if err != nil {
			conf.Log.Errorf("plugin %s can't create new control channel: %s", pluginMeta.Name, err.Error())
			return nil, fmt.Errorf("plugin %s can't create new control channel: %s", pluginMeta.Name, err.Error())
		}
		ins.ctrlChan = ctrlChan
		conf.Log.Infof("create process %s ctrl channel successfully", pluginMeta.Name)
	}
	defer func() {
		if e != nil && ins.ctrlChan != nil {
			ins.ctrlChan.Close()
		}
	}()
	// init or restart all need to run the process
	jsonArg, err := json.Marshal(pconf)
	if err != nil {
		conf.Log.Errorf("plugin %s invalid conf: %v", pluginMeta.Name, pconf)
		return nil, fmt.Errorf("invalid conf: %v", pconf)
	}
	var cmd *exec.Cmd
	err = infra.SafeRun(func() error {
		switch pluginMeta.Language {
		case "go":
			conf.Log.Printf("starting go plugin executable %s", pluginMeta.Executable)
			cmd = exec.Command(pluginMeta.Executable, string(jsonArg))

		case "python":
			if pluginMeta.VirtualType != "" {
				switch pluginMeta.VirtualType {
				case "conda":
					cmd = exec.Command("conda", "run", "-n", pluginMeta.Env, conf.Config.Portable.PythonBin, pluginMeta.Executable, string(jsonArg))
				default:
					return fmt.Errorf("unsupported virtual type: %s", pluginMeta.VirtualType)
				}
			}
			if cmd == nil {
				cmd = exec.Command(conf.Config.Portable.PythonBin, pluginMeta.Executable, string(jsonArg))
			}
			conf.Log.Infof("starting python plugin: %s", cmd)
		default:
			return fmt.Errorf("unsupported language: %s", pluginMeta.Language)
		}
		return nil
	})
	if err != nil {
		conf.Log.Errorf("failed to start plugin %s: %v", pluginMeta.Name, err)
		return nil, fmt.Errorf("fail to start plugin %s: %v", pluginMeta.Name, err)
	}
	cmd.Stdout = conf.Log.Out
	cmd.Stderr = conf.Log.Out
	cmd.Dir = filepath.Dir(pluginMeta.Executable)

	err = cmd.Start()
	if err != nil {
		conf.Log.Errorf("plugin %s executable %s stops with error %v", pluginMeta.Name, pluginMeta.Executable, err)
		return nil, fmt.Errorf("plugin %s executable %s stops with error %v", pluginMeta.Name, pluginMeta.Executable, err)
	}
	process := cmd.Process
	conf.Log.Printf("plugin %s started pid: %d\n", pluginMeta.Name, process.Pid)
	defer func() {
		if e != nil {
			_ = process.Kill()
		}
	}()
	go infra.SafeRun(func() error { // just print out error inside
		err = cmd.Wait()
		if err != nil {
			conf.Log.Errorf("plugin executable %s stops with error %v", pluginMeta.Executable, err)
		}
		// must make sure the plugin ins is not cleaned up yet by checking the process identity
		// clean up for stop unintentionally
		if ins, ok := p.getPluginIns(pluginMeta.Name); ok && ins.process == cmd.Process {
			ins.Lock()
			if ins.ctrlChan != nil {
				_ = ins.ctrlChan.Close()
			}
			ins.process = nil
			ins.Unlock()
			p.deletePluginIns(pluginMeta.Name)
		}
		return nil
	})
	err = ins.ctrlChan.Handshake()
	if err != nil {
		conf.Log.Infof("plugin %s handshake successfully", pluginMeta.Name)
		return nil, fmt.Errorf("plugin %s control handshake error: %v", pluginMeta.Executable, err)
	}
	conf.Log.Infof("plugin %s handshake successfully", pluginMeta.Name)
	ins.process = process
	p.instances[pluginMeta.Name] = ins
	conf.Log.Infof("plugin %s start running, process: %v", pluginMeta.Name, process.Pid)
	for key, jsonArg := range ins.commands {
		err := ins.sendCmd(jsonArg)
		if err != nil {
			conf.Log.Errorf("plugin %s send command key %s error:%v", pluginMeta.Name, key, err)
			return nil, err
		}
	}
	// restore symbols by sending commands when restarting plugin
	conf.Log.Infof("restore plugin %s symbols successfully", pluginMeta.Name)
	return ins, nil
}

func (p *pluginInsManager) Kill(name string) error {
	p.Lock()
	defer p.Unlock()
	var err error
	if ins, ok := p.instances[name]; ok {
		err = ins.Stop()
	} else {
		conf.Log.Warnf("instance %s not found when deleting", name)
		return nil
	}
	return err
}

func (p *pluginInsManager) KillAll() error {
	p.Lock()
	defer p.Unlock()
	for _, ins := range p.instances {
		_ = ins.Stop()
	}
	return nil
}

type PluginMeta struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Language    string `json:"language"`
	Executable  string `json:"executable"`
	VirtualType string `json:"virtualEnvType,omitempty"`
	Env         string `json:"env,omitempty"`
}
