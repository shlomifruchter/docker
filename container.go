package docker

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/dotcloud/docker/archive"
	"github.com/dotcloud/docker/term"
	"github.com/dotcloud/docker/utils"
	"github.com/kr/pty"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Container struct {
	root string

	ID string

	Created time.Time

	Path string
	Args []string

	Config *Config
	State  State
	Image  string

	network         *NetworkInterface
	NetworkSettings *NetworkSettings

	SysInitPath    string
	ResolvConfPath string
	HostnamePath   string
	HostsPath      string
	Name           string

	cmd       *exec.Cmd
	stdout    *utils.WriteBroadcaster
	stderr    *utils.WriteBroadcaster
	stdin     io.ReadCloser
	stdinPipe io.WriteCloser
	ptyMaster io.Closer

	runtime *Runtime

	waitLock chan struct{}
	Volumes  map[string]string
	// Store rw/ro in a separate structure to preserve reverse-compatibility on-disk.
	// Easier than migrating older container configs :)
	VolumesRW  map[string]bool
	hostConfig *HostConfig

	activeLinks map[string]*Link
}

// Note: the Config structure should hold only portable information about the container.
// Here, "portable" means "independent from the host we are running on".
// Non-portable information *should* appear in HostConfig.
type Config struct {
	Hostname        string
	Domainname      string
	User            string
	Memory          int64 // Memory limit (in bytes)
	MemorySwap      int64 // Total memory usage (memory + swap); set `-1' to disable swap
	CpuShares       int64 // CPU shares (relative weight vs. other containers)
	AttachStdin     bool
	AttachStdout    bool
	AttachStderr    bool
	PortSpecs       []string // Deprecated - Can be in the format of 8080/tcp
	ExposedPorts    map[Port]struct{}
	Tty             bool // Attach standard streams to a tty, including stdin if it is not closed.
	OpenStdin       bool // Open stdin
	StdinOnce       bool // If true, close stdin after the 1 attached client disconnects.
	Env             []string
	Cmd             []string
	Dns             []string
	Image           string // Name of the image as it was passed by the operator (eg. could be symbolic)
	Volumes         map[string]struct{}
	VolumesFrom     string
	WorkingDir      string
	Entrypoint      []string
	NetworkDisabled bool
}

type HostConfig struct {
	Binds           []string
	ContainerIDFile string
	LxcConf         []KeyValuePair
	Privileged      bool
	PortBindings    map[Port][]PortBinding
	Links           []string
	PublishAllPorts bool
}

type BindMap struct {
	SrcPath string
	DstPath string
	Mode    string
}

var (
	ErrContainerStart           = errors.New("The container failed to start. Unkown error")
	ErrContainerStartTimeout    = errors.New("The container failed to start due to timed out.")
	ErrInvalidWorikingDirectory = errors.New("The working directory is invalid. It needs to be an absolute path.")
	ErrConflictAttachDetach     = errors.New("Conflicting options: -a and -d")
	ErrConflictDetachAutoRemove = errors.New("Conflicting options: -rm and -d")
)

type KeyValuePair struct {
	Key   string
	Value string
}

type PortBinding struct {
	HostIp   string
	HostPort string
}

// 80/tcp
type Port string

func (p Port) Proto() string {
	parts := strings.Split(string(p), "/")
	if len(parts) == 1 {
		return "tcp"
	}
	return parts[1]
}

func (p Port) Port() string {
	return strings.Split(string(p), "/")[0]
}

func (p Port) Int() int {
	i, err := parsePort(p.Port())
	if err != nil {
		panic(err)
	}
	return i
}

func NewPort(proto, port string) Port {
	return Port(fmt.Sprintf("%s/%s", port, proto))
}

func ParseRun(args []string, capabilities *Capabilities) (*Config, *HostConfig, *flag.FlagSet, error) {
	cmd := Subcmd("run", "[OPTIONS] IMAGE [COMMAND] [ARG...]", "Run a command in a new container")
	if os.Getenv("TEST") != "" {
		cmd.SetOutput(ioutil.Discard)
		cmd.Usage = nil
	}

	flHostname := cmd.String("h", "", "Container host name")
	flWorkingDir := cmd.String("w", "", "Working directory inside the container")
	flUser := cmd.String("u", "", "Username or UID")
	flDetach := cmd.Bool("d", false, "Detached mode: Run container in the background, print new container id")
	flAttach := NewAttachOpts()
	cmd.Var(flAttach, "a", "Attach to stdin, stdout or stderr.")
	flStdin := cmd.Bool("i", false, "Keep stdin open even if not attached")
	flTty := cmd.Bool("t", false, "Allocate a pseudo-tty")
	flMemoryString := cmd.String("m", "", "Memory limit (format: <number><optional unit>, where unit = b, k, m or g)")
	flContainerIDFile := cmd.String("cidfile", "", "Write the container ID to the file")
	flNetwork := cmd.Bool("n", true, "Enable networking for this container")
	flPrivileged := cmd.Bool("privileged", false, "Give extended privileges to this container")
	flAutoRemove := cmd.Bool("rm", false, "Automatically remove the container when it exits (incompatible with -d)")
	cmd.Bool("sig-proxy", true, "Proxify all received signal to the process (even in non-tty mode)")
	cmd.String("name", "", "Assign a name to the container")
	flPublishAll := cmd.Bool("P", false, "Publish all exposed ports to the host interfaces")

	if capabilities != nil && *flMemoryString != "" && !capabilities.MemoryLimit {
		//fmt.Fprintf(stdout, "WARNING: Your kernel does not support memory limit capabilities. Limitation discarded.\n")
		*flMemoryString = ""
	}

	flCpuShares := cmd.Int64("c", 0, "CPU shares (relative weight)")

	var flPublish utils.ListOpts
	cmd.Var(&flPublish, "p", "Publish a container's port to the host (use 'docker port' to see the actual mapping)")

	var flExpose utils.ListOpts
	cmd.Var(&flExpose, "expose", "Expose a port from the container without publishing it to your host")

	var flEnv utils.ListOpts
	cmd.Var(&flEnv, "e", "Set environment variables")

	var flDns utils.ListOpts
	cmd.Var(&flDns, "dns", "Set custom dns servers")

	flVolumes := NewPathOpts()
	cmd.Var(flVolumes, "v", "Bind mount a volume (e.g. from the host: -v /host:/container, from docker: -v /container)")

	var flVolumesFrom utils.ListOpts
	cmd.Var(&flVolumesFrom, "volumes-from", "Mount volumes from the specified container(s)")

	flEntrypoint := cmd.String("entrypoint", "", "Overwrite the default entrypoint of the image")

	var flLxcOpts utils.ListOpts
	cmd.Var(&flLxcOpts, "lxc-conf", "Add custom lxc options -lxc-conf=\"lxc.cgroup.cpuset.cpus = 0,1\"")

	var flLinks utils.ListOpts
	cmd.Var(&flLinks, "link", "Add link to another container (name:alias)")

	if err := cmd.Parse(args); err != nil {
		return nil, nil, cmd, err
	}
	if *flDetach && len(flAttach) > 0 {
		return nil, nil, cmd, ErrConflictAttachDetach
	}
	if *flWorkingDir != "" && !path.IsAbs(*flWorkingDir) {
		return nil, nil, cmd, ErrInvalidWorikingDirectory
	}
	if *flDetach && *flAutoRemove {
		return nil, nil, cmd, ErrConflictDetachAutoRemove
	}

	// If neither -d or -a are set, attach to everything by default
	if len(flAttach) == 0 && !*flDetach {
		if !*flDetach {
			flAttach.Set("stdout")
			flAttach.Set("stderr")
			if *flStdin {
				flAttach.Set("stdin")
			}
		}
	}

	envs := []string{}

	for _, env := range flEnv {
		arr := strings.Split(env, "=")
		if len(arr) > 1 {
			envs = append(envs, env)
		} else {
			v := os.Getenv(env)
			envs = append(envs, env+"="+v)
		}
	}

	var flMemory int64

	if *flMemoryString != "" {
		parsedMemory, err := utils.RAMInBytes(*flMemoryString)

		if err != nil {
			return nil, nil, cmd, err
		}

		flMemory = parsedMemory
	}

	var binds []string

	// add any bind targets to the list of container volumes
	for bind := range flVolumes {
		arr := strings.Split(bind, ":")
		if len(arr) > 1 {
			if arr[0] == "/" {
				return nil, nil, cmd, fmt.Errorf("Invalid bind mount: source can't be '/'")
			}
			dstDir := arr[1]
			flVolumes[dstDir] = struct{}{}
			binds = append(binds, bind)
			delete(flVolumes, bind)
		}
	}

	parsedArgs := cmd.Args()
	runCmd := []string{}
	entrypoint := []string{}
	image := ""
	if len(parsedArgs) >= 1 {
		image = cmd.Arg(0)
	}
	if len(parsedArgs) > 1 {
		runCmd = parsedArgs[1:]
	}
	if *flEntrypoint != "" {
		entrypoint = []string{*flEntrypoint}
	}

	var lxcConf []KeyValuePair
	lxcConf, err := parseLxcConfOpts(flLxcOpts)
	if err != nil {
		return nil, nil, cmd, err
	}

	hostname := *flHostname
	domainname := ""

	parts := strings.SplitN(hostname, ".", 2)
	if len(parts) > 1 {
		hostname = parts[0]
		domainname = parts[1]
	}

	ports, portBindings, err := parsePortSpecs(flPublish)
	if err != nil {
		return nil, nil, cmd, err
	}

	// Merge in exposed ports to the map of published ports
	for _, e := range flExpose {
		if strings.Contains(e, ":") {
			return nil, nil, cmd, fmt.Errorf("Invalid port format for -expose: %s", e)
		}
		p := NewPort(splitProtoPort(e))
		if _, exists := ports[p]; !exists {
			ports[p] = struct{}{}
		}
	}

	config := &Config{
		Hostname:        hostname,
		Domainname:      domainname,
		PortSpecs:       nil, // Deprecated
		ExposedPorts:    ports,
		User:            *flUser,
		Tty:             *flTty,
		NetworkDisabled: !*flNetwork,
		OpenStdin:       *flStdin,
		Memory:          flMemory,
		CpuShares:       *flCpuShares,
		AttachStdin:     flAttach.Get("stdin"),
		AttachStdout:    flAttach.Get("stdout"),
		AttachStderr:    flAttach.Get("stderr"),
		Env:             envs,
		Cmd:             runCmd,
		Dns:             flDns,
		Image:           image,
		Volumes:         flVolumes,
		VolumesFrom:     strings.Join(flVolumesFrom, ","),
		Entrypoint:      entrypoint,
		WorkingDir:      *flWorkingDir,
	}

	hostConfig := &HostConfig{
		Binds:           binds,
		ContainerIDFile: *flContainerIDFile,
		LxcConf:         lxcConf,
		Privileged:      *flPrivileged,
		PortBindings:    portBindings,
		Links:           flLinks,
		PublishAllPorts: *flPublishAll,
	}

	if capabilities != nil && flMemory > 0 && !capabilities.SwapLimit {
		//fmt.Fprintf(stdout, "WARNING: Your kernel does not support swap limit capabilities. Limitation discarded.\n")
		config.MemorySwap = -1
	}

	// When allocating stdin in attached mode, close stdin at client disconnect
	if config.OpenStdin && config.AttachStdin {
		config.StdinOnce = true
	}
	return config, hostConfig, cmd, nil
}

type PortMapping map[string]string // Deprecated

type NetworkSettings struct {
	IPAddress   string
	IPPrefixLen int
	Gateway     string
	Bridge      string
	PortMapping map[string]PortMapping // Deprecated
	Ports       map[Port][]PortBinding
}

func (settings *NetworkSettings) PortMappingAPI() []APIPort {
	var mapping []APIPort
	for port, bindings := range settings.Ports {
		p, _ := parsePort(port.Port())
		if len(bindings) == 0 {
			mapping = append(mapping, APIPort{
				PublicPort: int64(p),
				Type:       port.Proto(),
			})
			continue
		}
		for _, binding := range bindings {
			p, _ := parsePort(port.Port())
			h, _ := parsePort(binding.HostPort)
			mapping = append(mapping, APIPort{
				PrivatePort: int64(p),
				PublicPort:  int64(h),
				Type:        port.Proto(),
				IP:          binding.HostIp,
			})
		}
	}
	return mapping
}

// Inject the io.Reader at the given path. Note: do not close the reader
func (container *Container) Inject(file io.Reader, pth string) error {
	// Return error if path exists
	if _, err := os.Stat(path.Join(container.rwPath(), pth)); err == nil {
		// Since err is nil, the path could be stat'd and it exists
		return fmt.Errorf("%s exists", pth)
	} else if !os.IsNotExist(err) {
		// Expect err might be that the file doesn't exist, so
		// if it's some other error, return that.

		return err
	}

	// Make sure the directory exists
	if err := os.MkdirAll(path.Join(container.rwPath(), path.Dir(pth)), 0755); err != nil {
		return err
	}

	dest, err := os.Create(path.Join(container.rwPath(), pth))
	if err != nil {
		return err
	}
	if _, err := io.Copy(dest, file); err != nil {
		return err
	}
	return nil
}

func (container *Container) Cmd() *exec.Cmd {
	return container.cmd
}

func (container *Container) When() time.Time {
	return container.Created
}

func (container *Container) FromDisk() error {
	data, err := ioutil.ReadFile(container.jsonPath())
	if err != nil {
		return err
	}
	// Load container settings
	// udp broke compat of docker.PortMapping, but it's not used when loading a container, we can skip it
	if err := json.Unmarshal(data, container); err != nil && !strings.Contains(err.Error(), "docker.PortMapping") {
		return err
	}
	return container.readHostConfig()
}

func (container *Container) ToDisk() (err error) {
	data, err := json.Marshal(container)
	if err != nil {
		return
	}
	err = ioutil.WriteFile(container.jsonPath(), data, 0666)
	if err != nil {
		return
	}
	return container.writeHostConfig()
}

func (container *Container) readHostConfig() error {
	container.hostConfig = &HostConfig{}
	// If the hostconfig file does not exist, do not read it.
	// (We still have to initialize container.hostConfig,
	// but that's OK, since we just did that above.)
	_, err := os.Stat(container.hostConfigPath())
	if os.IsNotExist(err) {
		return nil
	}
	data, err := ioutil.ReadFile(container.hostConfigPath())
	if err != nil {
		return err
	}
	return json.Unmarshal(data, container.hostConfig)
}

func (container *Container) writeHostConfig() (err error) {
	data, err := json.Marshal(container.hostConfig)
	if err != nil {
		return
	}
	return ioutil.WriteFile(container.hostConfigPath(), data, 0666)
}

func (container *Container) generateEnvConfig(env []string) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	ioutil.WriteFile(container.EnvConfigPath(), data, 0600)
	return nil
}

func (container *Container) generateLXCConfig() error {
	fo, err := os.Create(container.lxcConfigPath())
	if err != nil {
		return err
	}
	defer fo.Close()
	return LxcTemplateCompiled.Execute(fo, container)
}

func (container *Container) startPty() error {
	ptyMaster, ptySlave, err := pty.Open()
	if err != nil {
		return err
	}
	container.ptyMaster = ptyMaster
	container.cmd.Stdout = ptySlave
	container.cmd.Stderr = ptySlave

	// Copy the PTYs to our broadcasters
	go func() {
		defer container.stdout.CloseWriters()
		utils.Debugf("startPty: begin of stdout pipe")
		io.Copy(container.stdout, ptyMaster)
		utils.Debugf("startPty: end of stdout pipe")
	}()

	// stdin
	if container.Config.OpenStdin {
		container.cmd.Stdin = ptySlave
		container.cmd.SysProcAttr.Setctty = true
		go func() {
			defer container.stdin.Close()
			utils.Debugf("startPty: begin of stdin pipe")
			io.Copy(ptyMaster, container.stdin)
			utils.Debugf("startPty: end of stdin pipe")
		}()
	}
	if err := container.cmd.Start(); err != nil {
		return err
	}
	ptySlave.Close()
	return nil
}

func (container *Container) start() error {
	container.cmd.Stdout = container.stdout
	container.cmd.Stderr = container.stderr
	if container.Config.OpenStdin {
		stdin, err := container.cmd.StdinPipe()
		if err != nil {
			return err
		}
		go func() {
			defer stdin.Close()
			utils.Debugf("start: begin of stdin pipe")
			io.Copy(stdin, container.stdin)
			utils.Debugf("start: end of stdin pipe")
		}()
	}
	return container.cmd.Start()
}

func (container *Container) Attach(stdin io.ReadCloser, stdinCloser io.Closer, stdout io.Writer, stderr io.Writer) chan error {
	var cStdout, cStderr io.ReadCloser

	var nJobs int
	errors := make(chan error, 3)
	if stdin != nil && container.Config.OpenStdin {
		nJobs += 1
		if cStdin, err := container.StdinPipe(); err != nil {
			errors <- err
		} else {
			go func() {
				utils.Debugf("attach: stdin: begin")
				defer utils.Debugf("attach: stdin: end")
				// No matter what, when stdin is closed (io.Copy unblock), close stdout and stderr
				if container.Config.StdinOnce && !container.Config.Tty {
					defer cStdin.Close()
				} else {
					if cStdout != nil {
						defer cStdout.Close()
					}
					if cStderr != nil {
						defer cStderr.Close()
					}
				}
				if container.Config.Tty {
					_, err = utils.CopyEscapable(cStdin, stdin)
				} else {
					_, err = io.Copy(cStdin, stdin)
				}
				if err == io.ErrClosedPipe {
					err = nil
				}
				if err != nil {
					utils.Errorf("attach: stdin: %s", err)
				}
				errors <- err
			}()
		}
	}
	if stdout != nil {
		nJobs += 1
		if p, err := container.StdoutPipe(); err != nil {
			errors <- err
		} else {
			cStdout = p
			go func() {
				utils.Debugf("attach: stdout: begin")
				defer utils.Debugf("attach: stdout: end")
				// If we are in StdinOnce mode, then close stdin
				if container.Config.StdinOnce && stdin != nil {
					defer stdin.Close()
				}
				if stdinCloser != nil {
					defer stdinCloser.Close()
				}
				_, err := io.Copy(stdout, cStdout)
				if err == io.ErrClosedPipe {
					err = nil
				}
				if err != nil {
					utils.Errorf("attach: stdout: %s", err)
				}
				errors <- err
			}()
		}
	} else {
		go func() {
			if stdinCloser != nil {
				defer stdinCloser.Close()
			}
			if cStdout, err := container.StdoutPipe(); err != nil {
				utils.Errorf("attach: stdout pipe: %s", err)
			} else {
				io.Copy(&utils.NopWriter{}, cStdout)
			}
		}()
	}
	if stderr != nil {
		nJobs += 1
		if p, err := container.StderrPipe(); err != nil {
			errors <- err
		} else {
			cStderr = p
			go func() {
				utils.Debugf("attach: stderr: begin")
				defer utils.Debugf("attach: stderr: end")
				// If we are in StdinOnce mode, then close stdin
				if container.Config.StdinOnce && stdin != nil {
					defer stdin.Close()
				}
				if stdinCloser != nil {
					defer stdinCloser.Close()
				}
				_, err := io.Copy(stderr, cStderr)
				if err == io.ErrClosedPipe {
					err = nil
				}
				if err != nil {
					utils.Errorf("attach: stderr: %s", err)
				}
				errors <- err
			}()
		}
	} else {
		go func() {
			if stdinCloser != nil {
				defer stdinCloser.Close()
			}

			if cStderr, err := container.StderrPipe(); err != nil {
				utils.Errorf("attach: stdout pipe: %s", err)
			} else {
				io.Copy(&utils.NopWriter{}, cStderr)
			}
		}()
	}

	return utils.Go(func() error {
		if cStdout != nil {
			defer cStdout.Close()
		}
		if cStderr != nil {
			defer cStderr.Close()
		}
		// FIXME: how to clean up the stdin goroutine without the unwanted side effect
		// of closing the passed stdin? Add an intermediary io.Pipe?
		for i := 0; i < nJobs; i += 1 {
			utils.Debugf("attach: waiting for job %d/%d", i+1, nJobs)
			if err := <-errors; err != nil {
				utils.Errorf("attach: job %d returned error %s, aborting all jobs", i+1, err)
				return err
			}
			utils.Debugf("attach: job %d completed successfully", i+1)
		}
		utils.Debugf("attach: all jobs completed successfully")
		return nil
	})
}

func (container *Container) Start() (err error) {
	container.State.Lock()
	defer container.State.Unlock()
	if container.State.Running {
		return fmt.Errorf("The container %s is already running.", container.ID)
	}
	defer func() {
		if err != nil {
			container.cleanup()
		}
	}()
	if err := container.EnsureMounted(); err != nil {
		return err
	}
	if container.runtime.networkManager.disabled {
		container.Config.NetworkDisabled = true
		container.buildHostnameAndHostsFiles("127.0.1.1")
	} else {
		if err := container.allocateNetwork(); err != nil {
			return err
		}
		container.buildHostnameAndHostsFiles(container.NetworkSettings.IPAddress)
	}

	// Make sure the config is compatible with the current kernel
	if container.Config.Memory > 0 && !container.runtime.capabilities.MemoryLimit {
		log.Printf("WARNING: Your kernel does not support memory limit capabilities. Limitation discarded.\n")
		container.Config.Memory = 0
	}
	if container.Config.Memory > 0 && !container.runtime.capabilities.SwapLimit {
		log.Printf("WARNING: Your kernel does not support swap limit capabilities. Limitation discarded.\n")
		container.Config.MemorySwap = -1
	}

	if container.runtime.capabilities.IPv4ForwardingDisabled {
		log.Printf("WARNING: IPv4 forwarding is disabled. Networking will not work")
	}

	// Create the requested bind mounts
	binds := make(map[string]BindMap)
	// Define illegal container destinations
	illegalDsts := []string{"/", "."}

	for _, bind := range container.hostConfig.Binds {
		// FIXME: factorize bind parsing in parseBind
		var src, dst, mode string
		arr := strings.Split(bind, ":")
		if len(arr) == 2 {
			src = arr[0]
			dst = arr[1]
			mode = "rw"
		} else if len(arr) == 3 {
			src = arr[0]
			dst = arr[1]
			mode = arr[2]
		} else {
			return fmt.Errorf("Invalid bind specification: %s", bind)
		}

		// Bail if trying to mount to an illegal destination
		for _, illegal := range illegalDsts {
			if dst == illegal {
				return fmt.Errorf("Illegal bind destination: %s", dst)
			}
		}

		bindMap := BindMap{
			SrcPath: src,
			DstPath: dst,
			Mode:    mode,
		}
		binds[path.Clean(dst)] = bindMap
	}

	if container.Volumes == nil || len(container.Volumes) == 0 {
		container.Volumes = make(map[string]string)
		container.VolumesRW = make(map[string]bool)
	}

	// Apply volumes from another container if requested
	if container.Config.VolumesFrom != "" {
		containerSpecs := strings.Split(container.Config.VolumesFrom, ",")
		for _, containerSpec := range containerSpecs {
			mountRW := true
			specParts := strings.SplitN(containerSpec, ":", 2)
			switch len(specParts) {
			case 0:
				return fmt.Errorf("Malformed volumes-from specification: %s", container.Config.VolumesFrom)
			case 2:
				switch specParts[1] {
				case "ro":
					mountRW = false
				case "rw": // mountRW is already true
				default:
					return fmt.Errorf("Malformed volumes-from speficication: %s", containerSpec)
				}
			}
			c := container.runtime.Get(specParts[0])
			if c == nil {
				return fmt.Errorf("Container %s not found. Impossible to mount its volumes", container.ID)
			}
			for volPath, id := range c.Volumes {
				if _, exists := container.Volumes[volPath]; exists {
					continue
				}
				if err := os.MkdirAll(path.Join(container.RootfsPath(), volPath), 0755); err != nil {
					return err
				}
				container.Volumes[volPath] = id
				if isRW, exists := c.VolumesRW[volPath]; exists {
					container.VolumesRW[volPath] = isRW && mountRW
				}
			}

		}
	}

	// Create the requested volumes if they don't exist
	for volPath := range container.Config.Volumes {
		volPath = path.Clean(volPath)
		// Skip existing volumes
		if _, exists := container.Volumes[volPath]; exists {
			continue
		}
		var srcPath string
		var isBindMount bool
		srcRW := false
		// If an external bind is defined for this volume, use that as a source
		if bindMap, exists := binds[volPath]; exists {
			isBindMount = true
			srcPath = bindMap.SrcPath
			if strings.ToLower(bindMap.Mode) == "rw" {
				srcRW = true
			}
			// Otherwise create an directory in $ROOT/volumes/ and use that
		} else {
			c, err := container.runtime.volumes.Create(nil, container, "", "", nil)
			if err != nil {
				return err
			}
			srcPath, err = c.layer()
			if err != nil {
				return err
			}
			srcRW = true // RW by default
		}
		container.Volumes[volPath] = srcPath
		container.VolumesRW[volPath] = srcRW
		// Create the mountpoint
		rootVolPath := path.Join(container.RootfsPath(), volPath)
		if err := os.MkdirAll(rootVolPath, 0755); err != nil {
			return err
		}

		// Do not copy or change permissions if we are mounting from the host
		if srcRW && !isBindMount {
			volList, err := ioutil.ReadDir(rootVolPath)
			if err != nil {
				return err
			}
			if len(volList) > 0 {
				srcList, err := ioutil.ReadDir(srcPath)
				if err != nil {
					return err
				}
				if len(srcList) == 0 {
					// If the source volume is empty copy files from the root into the volume
					if err := archive.CopyWithTar(rootVolPath, srcPath); err != nil {
						return err
					}

					var stat syscall.Stat_t
					if err := syscall.Stat(rootVolPath, &stat); err != nil {
						return err
					}
					var srcStat syscall.Stat_t
					if err := syscall.Stat(srcPath, &srcStat); err != nil {
						return err
					}
					// Change the source volume's ownership if it differs from the root
					// files that where just copied
					if stat.Uid != srcStat.Uid || stat.Gid != srcStat.Gid {
						if err := os.Chown(srcPath, int(stat.Uid), int(stat.Gid)); err != nil {
							return err
						}
					}
				}
			}
		}
	}

	if err := container.generateLXCConfig(); err != nil {
		return err
	}

	var lxcStart string = "lxc-start"
	if container.hostConfig.Privileged && container.runtime.capabilities.AppArmor {
		lxcStart = path.Join(container.runtime.config.Root, "lxc-start-unconfined")
	}

	params := []string{
		lxcStart,
		"-n", container.ID,
		"-f", container.lxcConfigPath(),
		"--",
		"/.dockerinit",
	}

	// Networking
	if !container.Config.NetworkDisabled {
		params = append(params, "-g", container.network.Gateway.String())
	}

	// User
	if container.Config.User != "" {
		params = append(params, "-u", container.Config.User)
	}

	// Setup environment
	env := []string{
		"HOME=/",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"container=lxc",
		"HOSTNAME=" + container.Config.Hostname,
	}

	if container.Config.Tty {
		env = append(env, "TERM=xterm")
	}

	// Init any links between the parent and children
	runtime := container.runtime

	children, err := runtime.Children(container.Name)
	if err != nil {
		return err
	}

	if len(children) > 0 {
		container.activeLinks = make(map[string]*Link, len(children))

		// If we encounter an error make sure that we rollback any network
		// config and ip table changes
		rollback := func() {
			for _, link := range container.activeLinks {
				link.Disable()
			}
			container.activeLinks = nil
		}

		for p, child := range children {
			link, err := NewLink(container, child, p, runtime.networkManager.bridgeIface)
			if err != nil {
				rollback()
				return err
			}

			container.activeLinks[link.Alias()] = link
			if err := link.Enable(); err != nil {
				rollback()
				return err
			}

			for _, envVar := range link.ToEnv() {
				env = append(env, envVar)
			}
		}
	}

	for _, elem := range container.Config.Env {
		env = append(env, elem)
	}

	if err := container.generateEnvConfig(env); err != nil {
		return err
	}

	if container.Config.WorkingDir != "" {
		workingDir := path.Clean(container.Config.WorkingDir)
		utils.Debugf("[working dir] working dir is %s", workingDir)

		if err := os.MkdirAll(path.Join(container.RootfsPath(), workingDir), 0755); err != nil {
			return nil
		}

		params = append(params,
			"-w", workingDir,
		)
	}

	// Program
	params = append(params, "--", container.Path)
	params = append(params, container.Args...)

	if RootIsShared() {
		// lxc-start really needs / to be non-shared, or all kinds of stuff break
		// when lxc-start unmount things and those unmounts propagate to the main
		// mount namespace.
		// What we really want is to clone into a new namespace and then
		// mount / MS_REC|MS_SLAVE, but since we can't really clone or fork
		// without exec in go we have to do this horrible shell hack...
		shellString :=
			"mount --make-rslave /; exec " +
				utils.ShellQuoteArguments(params)

		params = []string{
			"unshare", "-m", "--", "/bin/sh", "-c", shellString,
		}
	}

	container.cmd = exec.Command(params[0], params[1:]...)

	// Setup logging of stdout and stderr to disk
	if err := container.runtime.LogToDisk(container.stdout, container.logPath("json"), "stdout"); err != nil {
		return err
	}
	if err := container.runtime.LogToDisk(container.stderr, container.logPath("json"), "stderr"); err != nil {
		return err
	}

	container.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if container.Config.Tty {
		err = container.startPty()
	} else {
		err = container.start()
	}
	if err != nil {
		return err
	}
	// FIXME: save state on disk *first*, then converge
	// this way disk state is used as a journal, eg. we can restore after crash etc.
	container.State.setRunning(container.cmd.Process.Pid)

	// Init the lock
	container.waitLock = make(chan struct{})

	container.ToDisk()
	go container.monitor()

	defer utils.Debugf("Container running: %v", container.State.Running)
	// We wait for the container to be fully running.
	// Timeout after 5 seconds. In case of broken pipe, just retry.
	// Note: The container can run and finish correctly before
	//       the end of this loop
	for now := time.Now(); time.Since(now) < 5*time.Second; {
		// If the container dies while waiting for it, just return
		if !container.State.Running {
			return nil
		}
		output, err := exec.Command("lxc-info", "-s", "-n", container.ID).CombinedOutput()
		if err != nil {
			utils.Debugf("Error with lxc-info: %s (%s)", err, output)

			output, err = exec.Command("lxc-info", "-s", "-n", container.ID).CombinedOutput()
			if err != nil {
				utils.Debugf("Second Error with lxc-info: %s (%s)", err, output)
				return err
			}

		}
		if strings.Contains(string(output), "RUNNING") {
			return nil
		}
		utils.Debugf("Waiting for the container to start (running: %v): %s", container.State.Running, bytes.TrimSpace(output))
		time.Sleep(50 * time.Millisecond)
	}

	if container.State.Running {
		return ErrContainerStartTimeout
	}
	return ErrContainerStart
}

func (container *Container) Run() error {
	if err := container.Start(); err != nil {
		return err
	}
	container.Wait()
	return nil
}

func (container *Container) Output() (output []byte, err error) {
	pipe, err := container.StdoutPipe()
	if err != nil {
		return nil, err
	}
	defer pipe.Close()
	if err := container.Start(); err != nil {
		return nil, err
	}
	output, err = ioutil.ReadAll(pipe)
	container.Wait()
	return output, err
}

// Container.StdinPipe returns a WriteCloser which can be used to feed data
// to the standard input of the container's active process.
// Container.StdoutPipe and Container.StderrPipe each return a ReadCloser
// which can be used to retrieve the standard output (and error) generated
// by the container's active process. The output (and error) are actually
// copied and delivered to all StdoutPipe and StderrPipe consumers, using
// a kind of "broadcaster".

func (container *Container) StdinPipe() (io.WriteCloser, error) {
	return container.stdinPipe, nil
}

func (container *Container) StdoutPipe() (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	container.stdout.AddWriter(writer, "")
	return utils.NewBufReader(reader), nil
}

func (container *Container) StderrPipe() (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	container.stderr.AddWriter(writer, "")
	return utils.NewBufReader(reader), nil
}

func (container *Container) buildHostnameAndHostsFiles(IP string) {
	container.HostnamePath = path.Join(container.root, "hostname")
	ioutil.WriteFile(container.HostnamePath, []byte(container.Config.Hostname+"\n"), 0644)

	hostsContent := []byte(`
127.0.0.1	localhost
::1		localhost ip6-localhost ip6-loopback
fe00::0		ip6-localnet
ff00::0		ip6-mcastprefix
ff02::1		ip6-allnodes
ff02::2		ip6-allrouters
`)

	container.HostsPath = path.Join(container.root, "hosts")

	if container.Config.Domainname != "" {
		hostsContent = append([]byte(fmt.Sprintf("%s\t%s.%s %s\n", IP, container.Config.Hostname, container.Config.Domainname, container.Config.Hostname)), hostsContent...)
	} else {
		hostsContent = append([]byte(fmt.Sprintf("%s\t%s\n", IP, container.Config.Hostname)), hostsContent...)
	}

	ioutil.WriteFile(container.HostsPath, hostsContent, 0644)
}

func (container *Container) allocateNetwork() error {
	if container.Config.NetworkDisabled {
		return nil
	}

	var iface *NetworkInterface
	var err error
	if container.State.Ghost {
		manager := container.runtime.networkManager
		if manager.disabled {
			iface = &NetworkInterface{disabled: true}
		} else {
			iface = &NetworkInterface{
				IPNet:   net.IPNet{IP: net.ParseIP(container.NetworkSettings.IPAddress), Mask: manager.bridgeNetwork.Mask},
				Gateway: manager.bridgeNetwork.IP,
				manager: manager,
			}
			if iface != nil && iface.IPNet.IP != nil {
				ipNum := ipToInt(iface.IPNet.IP)
				manager.ipAllocator.inUse[ipNum] = struct{}{}
			} else {
				iface, err = container.runtime.networkManager.Allocate()
				if err != nil {
					return err
				}
			}
		}
	} else {
		iface, err = container.runtime.networkManager.Allocate()
		if err != nil {
			return err
		}
	}

	if container.Config.PortSpecs != nil {
		utils.Debugf("Migrating port mappings for container: %s", strings.Join(container.Config.PortSpecs, ", "))
		if err := migratePortMappings(container.Config, container.hostConfig); err != nil {
			return err
		}
		container.Config.PortSpecs = nil
		if err := container.writeHostConfig(); err != nil {
			return err
		}
	}

	portSpecs := make(map[Port]struct{})
	bindings := make(map[Port][]PortBinding)

	if !container.State.Ghost {
		if container.Config.ExposedPorts != nil {
			portSpecs = container.Config.ExposedPorts
		}
		if container.hostConfig.PortBindings != nil {
			bindings = container.hostConfig.PortBindings
		}
	} else {
		if container.NetworkSettings.Ports != nil {
			for port, binding := range container.NetworkSettings.Ports {
				portSpecs[port] = struct{}{}
				bindings[port] = binding
			}
		}
	}

	container.NetworkSettings.PortMapping = nil

	for port := range portSpecs {
		binding := bindings[port]
		if container.hostConfig.PublishAllPorts && len(binding) == 0 {
			binding = append(binding, PortBinding{})
		}
		for i := 0; i < len(binding); i++ {
			b := binding[i]
			nat, err := iface.AllocatePort(port, b)
			if err != nil {
				iface.Release()
				return err
			}
			utils.Debugf("Allocate port: %s:%s->%s", nat.Binding.HostIp, port, nat.Binding.HostPort)
			binding[i] = nat.Binding
		}
		bindings[port] = binding
	}
	container.writeHostConfig()

	container.NetworkSettings.Ports = bindings
	container.network = iface

	container.NetworkSettings.Bridge = container.runtime.networkManager.bridgeIface
	container.NetworkSettings.IPAddress = iface.IPNet.IP.String()
	container.NetworkSettings.IPPrefixLen, _ = iface.IPNet.Mask.Size()
	container.NetworkSettings.Gateway = iface.Gateway.String()

	return nil
}

func (container *Container) releaseNetwork() {
	if container.Config.NetworkDisabled || container.network == nil {
		return
	}
	container.network.Release()
	container.network = nil
	container.NetworkSettings = &NetworkSettings{}
}

// FIXME: replace this with a control socket within dockerinit
func (container *Container) waitLxc() error {
	for {
		output, err := exec.Command("lxc-info", "-n", container.ID).CombinedOutput()
		if err != nil {
			return err
		}
		if !strings.Contains(string(output), "RUNNING") {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (container *Container) monitor() {
	// Wait for the program to exit

	// If the command does not exist, try to wait via lxc
	// (This probably happens only for ghost containers, i.e. containers that were running when Docker started)
	if container.cmd == nil {
		utils.Debugf("monitor: waiting for container %s using waitLxc", container.ID)
		if err := container.waitLxc(); err != nil {
			utils.Errorf("monitor: while waiting for container %s, waitLxc had a problem: %s", container.ID, err)
		}
	} else {
		utils.Debugf("monitor: waiting for container %s using cmd.Wait", container.ID)
		if err := container.cmd.Wait(); err != nil {
			// Since non-zero exit status and signal terminations will cause err to be non-nil,
			// we have to actually discard it. Still, log it anyway, just in case.
			utils.Debugf("monitor: cmd.Wait reported exit status %s for container %s", err, container.ID)
		}
	}
	utils.Debugf("monitor: container %s finished", container.ID)

	exitCode := -1
	if container.cmd != nil {
		exitCode = container.cmd.ProcessState.Sys().(syscall.WaitStatus).ExitStatus()
	}

	// Report status back
	container.State.setStopped(exitCode)

	if container.runtime != nil && container.runtime.srv != nil {
		container.runtime.srv.LogEvent("die", container.ID, container.runtime.repositories.ImageName(container.Image))
	}

	// Cleanup
	container.cleanup()

	// Re-create a brand new stdin pipe once the container exited
	if container.Config.OpenStdin {
		container.stdin, container.stdinPipe = io.Pipe()
	}

	// Release the lock
	close(container.waitLock)

	if err := container.ToDisk(); err != nil {
		// FIXME: there is a race condition here which causes this to fail during the unit tests.
		// If another goroutine was waiting for Wait() to return before removing the container's root
		// from the filesystem... At this point it may already have done so.
		// This is because State.setStopped() has already been called, and has caused Wait()
		// to return.
		// FIXME: why are we serializing running state to disk in the first place?
		//log.Printf("%s: Failed to dump configuration to the disk: %s", container.ID, err)
	}
}

func (container *Container) cleanup() {
	container.releaseNetwork()

	// Disable all active links
	if container.activeLinks != nil {
		for _, link := range container.activeLinks {
			link.Disable()
		}
	}

	if container.Config.OpenStdin {
		if err := container.stdin.Close(); err != nil {
			utils.Errorf("%s: Error close stdin: %s", container.ID, err)
		}
	}
	if err := container.stdout.CloseWriters(); err != nil {
		utils.Errorf("%s: Error close stdout: %s", container.ID, err)
	}
	if err := container.stderr.CloseWriters(); err != nil {
		utils.Errorf("%s: Error close stderr: %s", container.ID, err)
	}

	if container.ptyMaster != nil {
		if err := container.ptyMaster.Close(); err != nil {
			utils.Errorf("%s: Error closing Pty master: %s", container.ID, err)
		}
	}

	if err := container.Unmount(); err != nil {
		log.Printf("%v: Failed to umount filesystem: %v", container.ID, err)
	}
}

func (container *Container) kill(sig int) error {
	container.State.Lock()
	defer container.State.Unlock()

	if !container.State.Running {
		return nil
	}

	if output, err := exec.Command("lxc-kill", "-n", container.ID, strconv.Itoa(sig)).CombinedOutput(); err != nil {
		log.Printf("error killing container %s (%s, %s)", utils.TruncateID(container.ID), output, err)
		return err
	}

	return nil
}

func (container *Container) Kill() error {
	if !container.State.Running {
		return nil
	}

	// 1. Send SIGKILL
	if err := container.kill(9); err != nil {
		return err
	}

	// 2. Wait for the process to die, in last resort, try to kill the process directly
	if err := container.WaitTimeout(10 * time.Second); err != nil {
		if container.cmd == nil {
			return fmt.Errorf("lxc-kill failed, impossible to kill the container %s", utils.TruncateID(container.ID))
		}
		log.Printf("Container %s failed to exit within 10 seconds of lxc-kill %s - trying direct SIGKILL", "SIGKILL", utils.TruncateID(container.ID))
		if err := container.cmd.Process.Kill(); err != nil {
			return err
		}
	}

	container.Wait()
	return nil
}

func (container *Container) Stop(seconds int) error {
	if !container.State.Running {
		return nil
	}

	// 1. Send a SIGTERM
	if err := container.kill(15); err != nil {
		utils.Debugf("Error sending kill SIGTERM: %s", err)
		log.Print("Failed to send SIGTERM to the process, force killing")
		if err := container.kill(9); err != nil {
			return err
		}
	}

	// 2. Wait for the process to exit on its own
	if err := container.WaitTimeout(time.Duration(seconds) * time.Second); err != nil {
		log.Printf("Container %v failed to exit within %d seconds of SIGTERM - using the force", container.ID, seconds)
		// 3. If it doesn't, then send SIGKILL
		if err := container.Kill(); err != nil {
			return err
		}
	}
	return nil
}

func (container *Container) Restart(seconds int) error {
	if err := container.Stop(seconds); err != nil {
		return err
	}
	return container.Start()
}

// Wait blocks until the container stops running, then returns its exit code.
func (container *Container) Wait() int {
	<-container.waitLock
	return container.State.ExitCode
}

func (container *Container) Resize(h, w int) error {
	pty, ok := container.ptyMaster.(*os.File)
	if !ok {
		return fmt.Errorf("ptyMaster does not have Fd() method")
	}
	return term.SetWinsize(pty.Fd(), &term.Winsize{Height: uint16(h), Width: uint16(w)})
}

func (container *Container) ExportRw() (archive.Archive, error) {
	return archive.Tar(container.rwPath(), archive.Uncompressed)
}

func (container *Container) RwChecksum() (string, error) {
	rwData, err := archive.Tar(container.rwPath(), archive.Xz)
	if err != nil {
		return "", err
	}
	return utils.HashData(rwData)
}

func (container *Container) Export() (archive.Archive, error) {
	if err := container.EnsureMounted(); err != nil {
		return nil, err
	}
	return archive.Tar(container.RootfsPath(), archive.Uncompressed)
}

func (container *Container) WaitTimeout(timeout time.Duration) error {
	done := make(chan bool)
	go func() {
		container.Wait()
		done <- true
	}()

	select {
	case <-time.After(timeout):
		return fmt.Errorf("Timed Out")
	case <-done:
		return nil
	}
}

func (container *Container) EnsureMounted() error {
	if mounted, err := container.Mounted(); err != nil {
		return err
	} else if mounted {
		return nil
	}
	return container.Mount()
}

func (container *Container) Mount() error {
	image, err := container.GetImage()
	if err != nil {
		return err
	}
	return image.Mount(container.RootfsPath(), container.rwPath())
}

func (container *Container) Changes() ([]Change, error) {
	image, err := container.GetImage()
	if err != nil {
		return nil, err
	}
	return image.Changes(container.rwPath())
}

func (container *Container) GetImage() (*Image, error) {
	if container.runtime == nil {
		return nil, fmt.Errorf("Can't get image of unregistered container")
	}
	return container.runtime.graph.Get(container.Image)
}

func (container *Container) Mounted() (bool, error) {
	return Mounted(container.RootfsPath())
}

func (container *Container) Unmount() error {
	if _, err := os.Stat(container.RootfsPath()); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return Unmount(container.RootfsPath())
}

func (container *Container) logPath(name string) string {
	return path.Join(container.root, fmt.Sprintf("%s-%s.log", container.ID, name))
}

func (container *Container) ReadLog(name string) (io.Reader, error) {
	return os.Open(container.logPath(name))
}

func (container *Container) hostConfigPath() string {
	return path.Join(container.root, "hostconfig.json")
}

func (container *Container) jsonPath() string {
	return path.Join(container.root, "config.json")
}

func (container *Container) EnvConfigPath() string {
	return path.Join(container.root, "config.env")
}

func (container *Container) lxcConfigPath() string {
	return path.Join(container.root, "config.lxc")
}

// This method must be exported to be used from the lxc template
func (container *Container) RootfsPath() string {
	return path.Join(container.root, "rootfs")
}

func (container *Container) rwPath() string {
	return path.Join(container.root, "rw")
}

func validateID(id string) error {
	if id == "" {
		return fmt.Errorf("Invalid empty id")
	}
	return nil
}

// GetSize, return real size, virtual size
func (container *Container) GetSize() (int64, int64) {
	var sizeRw, sizeRootfs int64

	filepath.Walk(container.rwPath(), func(path string, fileInfo os.FileInfo, err error) error {
		if fileInfo != nil {
			sizeRw += fileInfo.Size()
		}
		return nil
	})

	_, err := os.Stat(container.RootfsPath())
	if err == nil {
		filepath.Walk(container.RootfsPath(), func(path string, fileInfo os.FileInfo, err error) error {
			if fileInfo != nil {
				sizeRootfs += fileInfo.Size()
			}
			return nil
		})
	}
	return sizeRw, sizeRootfs
}

func (container *Container) Copy(resource string) (archive.Archive, error) {
	if err := container.EnsureMounted(); err != nil {
		return nil, err
	}
	var filter []string
	basePath := path.Join(container.RootfsPath(), resource)
	stat, err := os.Stat(basePath)
	if err != nil {
		return nil, err
	}
	if !stat.IsDir() {
		d, f := path.Split(basePath)
		basePath = d
		filter = []string{f}
	} else {
		filter = []string{path.Base(basePath)}
		basePath = path.Dir(basePath)
	}
	return archive.TarFilter(basePath, archive.Uncompressed, filter)
}

// Returns true if the container exposes a certain port
func (container *Container) Exposes(p Port) bool {
	_, exists := container.Config.ExposedPorts[p]
	return exists
}
