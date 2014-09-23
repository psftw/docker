package virtualbox

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/hosts/ssh"
	"github.com/docker/docker/hosts/state"
	"github.com/docker/docker/pkg/log"
	"github.com/docker/docker/utils"
)

type Driver struct {
	machineName string
	sshPort     uint
	memory      uint // main memory (in MB)
	diskSize    uint // in GB
	storePath   string
}

func NewDriver(options map[string]string, storePath string) (*Driver, error) {
	driver := &Driver{storePath: storePath}
	driver.LoadOptions(options)
	return driver, nil
}

func (d *Driver) DriverName() string {
	return "virtualbox"
}

func (d *Driver) GetOptions() map[string]string {
	return map[string]string{"machineName": d.machineName}
}

func (d *Driver) LoadOptions(options map[string]string) {
	d.machineName = options["machineName"]
	// d.memory = options["memory"]
	// if d.memory == nil {
	d.memory = 1024
	// }
	d.sshPort = 2223
	d.diskSize = 20000
}

func (d *Driver) GetURL() (string, error) {
	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("tcp://%s:2375", ip), nil
}

func (d *Driver) Create() error {
	d.setMachineNameIfNotSet()

	isISODownloaded, err := d.isISODownloaded()
	if err != nil {
		return err
	}
	if !isISODownloaded {
		tag, err := getLatestReleaseName()
		if err != nil {
			return err
		}
		log.Infof("Downloading boot2docker %s...", tag)
		if err := downloadISO(path.Join(d.storePath, "boot2docker.iso"), tag); err != nil {
			return err
		}
	}

	if err := ssh.GenerateSSHKey(d.sshKeyPath()); err != nil {
		return err
	}

	if err := d.generateDiskImage(d.diskSize); err != nil {
		return err
	}

	if err := vbm("createvm",
		"--name", d.machineName,
		"--register"); err != nil {
		return err
	}

	cpus := uint(runtime.NumCPU())
	if cpus > 32 {
		cpus = 32
	}

	if err := vbm("modifyvm", d.machineName,
		"--firmware", "bios",
		"--bioslogofadein", "off",
		"--bioslogofadeout", "off",
		"--natdnshostresolver1", "on",
		"--bioslogodisplaytime", "0",
		"--biosbootmenu", "disabled",

		"--ostype", "Linux26_64",
		"--cpus", fmt.Sprintf("%d", cpus),
		"--memory", fmt.Sprintf("%d", d.memory),

		"--acpi", "on",
		"--ioapic", "on",
		"--rtcuseutc", "on",
		"--cpuhotplug", "off",
		"--pae", "on",
		"--longmode", "on",
		"--synthcpu", "off",
		"--hpet", "on",
		"--hwvirtex", "on",
		"--triplefaultreset", "off",
		"--nestedpaging", "on",
		"--largepages", "on",
		"--vtxvpid", "on",
		"--vtxux", "off",
		"--accelerate3d", "off",
		"--boot1", "dvd"); err != nil {
		return err
	}

	if err := vbm("modifyvm", d.machineName,
		"--nic1", "nat",
		"--nictype1", "virtio",
		"--cableconnected1", "on"); err != nil {
		return err
	}

	if err := vbm("modifyvm", d.machineName,
		"--natpf1", fmt.Sprintf("ssh,tcp,127.0.0.1,%d,,22", d.sshPort)); err != nil {
		return err
	}

	hostOnlyNetwork, err := getOrCreateHostOnlyNetwork(
		net.ParseIP("192.168.99.1"),
		net.IPv4Mask(255, 255, 255, 0),
		net.ParseIP("192.168.99.2"),
		net.ParseIP("192.168.99.100"),
		net.ParseIP("192.168.99.254"))
	if err != nil {
		return err
	}
	if err := vbm("modifyvm", d.machineName,
		"--nic2", "hostonly",
		"--nictype2", "virtio",
		"--hostonlyadapter2", hostOnlyNetwork.Name,
		"--cableconnected2", "on"); err != nil {
		return err
	}

	if err := vbm("storagectl", d.machineName,
		"--name", "SATA",
		"--add", "sata",
		"--hostiocache", "on"); err != nil {
		return err
	}

	if err := vbm("storageattach", d.machineName,
		"--storagectl", "SATA",
		"--port", "0",
		"--device", "0",
		"--type", "dvddrive",
		"--medium", path.Join(d.storePath, "boot2docker.iso")); err != nil {
		return err
	}

	if err := vbm("storageattach", d.machineName,
		"--storagectl", "SATA",
		"--port", "1",
		"--device", "0",
		"--type", "hdd",
		"--medium", d.diskPath()); err != nil {
		return err
	}

	return nil
}

func (d *Driver) Start() error {
	if err := vbm("startvm", d.machineName, "--type", "headless"); err != nil {
		return err
	}
	log.Infof("Waiting for host to start...")
	return ssh.WaitForTCP(fmt.Sprintf("localhost:%d", d.sshPort))
}

func (d *Driver) Stop() error {
	if err := vbm("controlvm", d.machineName, "acpipowerbutton"); err != nil {
		return err
	}
	for {
		s, err := d.GetState()
		if err != nil {
			return err
		}
		if s == state.Running {
			time.Sleep(1 * time.Second)
		} else {
			break
		}
	}
	return nil
}

func (d *Driver) Remove() error {
	s, err := d.GetState()
	if err != nil {
		return err
	}
	if s == state.Running {
		if err := d.Kill(); err != nil {
			return err
		}
	}
	return vbm("unregistervm", "--delete", d.machineName)
}

func (d *Driver) Restart() error {
	if err := d.Stop(); err != nil {
		return err
	}
	return d.Start()
}

func (d *Driver) Kill() error {
	return vbm("controlvm", d.machineName, "poweroff")
}

func (d *Driver) GetState() (state.State, error) {
	stdout, stderr, err := vbmOutErr("showvminfo", d.machineName,
		"--machinereadable")
	if err != nil {
		if reMachineNotFound.FindString(stderr) != "" {
			return state.Unknown, ErrMachineNotExist
		}
		return state.Unknown, err
	}
	re := regexp.MustCompile(`(?m)^VMState="(\w+)"$`)
	groups := re.FindStringSubmatch(stdout)
	if len(groups) < 1 {
		return state.Unknown, nil
	}
	switch groups[1] {
	case "running":
		return state.Running, nil
	case "paused":
		return state.Paused, nil
	case "saved":
		return state.Saved, nil
	case "poweroff", "aborted":
		return state.Stopped, nil
	}
	return state.Unknown, nil
}

func (d *Driver) setMachineNameIfNotSet() {
	if d.machineName == "" {
		d.machineName = fmt.Sprintf("docker-host-%s", utils.GenerateRandomID())
	}
}

func (d *Driver) isISODownloaded() (bool, error) {
	if _, err := os.Stat(path.Join(d.storePath, "boot2docker.iso")); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (d *Driver) GetIP() (string, error) {
	cmd := d.GetSSHCommand("ip addr show dev eth1")

	b, err := cmd.Output()
	if err != nil {
		return "", err
	}
	out := string(b)
	log.Debugf("SSH returned: %s\nEND SSH\n", out)
	// parse to find: inet 192.168.59.103/24 brd 192.168.59.255 scope global eth1
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		vals := strings.Split(strings.TrimSpace(line), " ")
		if len(vals) >= 2 && vals[0] == "inet" {
			return vals[1][:strings.Index(vals[1], "/")], nil
		}
	}

	return "", fmt.Errorf("No IP address found %s", out)
}

func (d *Driver) GetSSHCommand(args ...string) *exec.Cmd {
	return ssh.GetSSHCommand("localhost", d.sshPort, "docker", d.sshKeyPath(), args...)
}

func (d *Driver) sshKeyPath() string {
	return path.Join(d.storePath, "id_rsa")
}

func (d *Driver) publicSSHKeyPath() string {
	return d.sshKeyPath() + ".pub"
}

func (d *Driver) diskPath() string {
	return path.Join(d.storePath, "disk.vmdk")
}

// Get the latest boot2docker release tag name (e.g. "v0.6.0").
func getLatestReleaseName() (string, error) {
	rsp, err := http.Get("https://api.github.com/repos/boot2docker/boot2docker/releases")
	if err != nil {
		return "", err
	}
	defer rsp.Body.Close()

	var t []struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(rsp.Body).Decode(&t); err != nil {
		return "", err
	}
	if len(t) == 0 {
		return "", fmt.Errorf("no releases found")
	}
	return t[0].TagName, nil
}

// Download boot2docker ISO image for the given tag and save it at dest.
func downloadISO(dest, tag string) error {
	rsp, err := http.Get(fmt.Sprintf("https://github.com/boot2docker/boot2docker/releases/download/%s/boot2docker.iso", tag))
	if err != nil {
		return err
	}
	defer rsp.Body.Close()

	// Download to a temp file first then rename it to avoid partial download.
	f, err := ioutil.TempFile("", "boot2docker-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, rsp.Body); err != nil {
		// TODO: display download progress?
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), dest); err != nil {
		return err
	}
	return nil
}

// Make a boot2docker VM disk image.
func (d *Driver) generateDiskImage(size uint) error {
	log.Debugf("Creating %d MB hard disk image...", size)

	magicString := "boot2docker, please format-me"

	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)

	// magicString first so the automount script knows to format the disk
	file := &tar.Header{Name: magicString, Size: int64(len(magicString))}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(magicString)); err != nil {
		return err
	}
	// .ssh/key.pub => authorized_keys
	file = &tar.Header{Name: ".ssh", Typeflag: tar.TypeDir, Mode: 0700}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	pubKey, err := ioutil.ReadFile(d.publicSSHKeyPath())
	if err != nil {
		return err
	}
	file = &tar.Header{Name: ".ssh/authorized_keys", Size: int64(len(pubKey)), Mode: 0644}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(pubKey)); err != nil {
		return err
	}
	file = &tar.Header{Name: ".ssh/authorized_keys2", Size: int64(len(pubKey)), Mode: 0644}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(pubKey)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	raw := bytes.NewReader(buf.Bytes())
	return createDiskImage(d.diskPath(), size, raw)
}

// createDiskImage makes a disk image at dest with the given size in MB. If r is
// not nil, it will be read as a raw disk image to convert from.
func createDiskImage(dest string, size uint, r io.Reader) error {
	// Convert a raw image from stdin to the dest VMDK image.
	sizeBytes := int64(size) << 20 // usually won't fit in 32-bit int (max 2GB)
	cmd := exec.Command(VBM, "convertfromraw", "stdin", dest,
		fmt.Sprintf("%d", sizeBytes), "--format", "VMDK")

	if os.Getenv("DEBUG") != "" {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	n, err := io.Copy(stdin, r)
	if err != nil {
		return err
	}

	// The total number of bytes written to stdin must match sizeBytes, or
	// VBoxManage.exe on Windows will fail. Fill remaining with zeros.
	if left := sizeBytes - n; left > 0 {
		if err := zeroFill(stdin, left); err != nil {
			return err
		}
	}

	// cmd won't exit until the stdin is closed.
	if err := stdin.Close(); err != nil {
		return err
	}

	return cmd.Wait()
}

// zeroFill writes n zero bytes into w.
func zeroFill(w io.Writer, n int64) error {
	const blocksize = 32 << 10
	zeros := make([]byte, blocksize)
	var k int
	var err error
	for n > 0 {
		if n > blocksize {
			k, err = w.Write(zeros)
		} else {
			k, err = w.Write(zeros[:n])
		}
		if err != nil {
			return err
		}
		n -= int64(k)
	}
	return nil
}
