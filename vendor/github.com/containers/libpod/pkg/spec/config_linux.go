// +build linux

package createconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/devices"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

// Device transforms a libcontainer configs.Device to a specs.LinuxDevice object.
func Device(d *configs.Device) spec.LinuxDevice {
	return spec.LinuxDevice{
		Type:     string(d.Type),
		Path:     d.Path,
		Major:    d.Major,
		Minor:    d.Minor,
		FileMode: fmPtr(int64(d.FileMode)),
		UID:      u32Ptr(int64(d.Uid)),
		GID:      u32Ptr(int64(d.Gid)),
	}
}

// devicesFromPath computes a list of devices
func devicesFromPath(g *generate.Generator, devicePath string) error {
	devs := strings.Split(devicePath, ":")
	resolvedDevicePath := devs[0]
	// check if it is a symbolic link
	if src, err := os.Lstat(resolvedDevicePath); err == nil && src.Mode()&os.ModeSymlink == os.ModeSymlink {
		if linkedPathOnHost, err := filepath.EvalSymlinks(resolvedDevicePath); err == nil {
			resolvedDevicePath = linkedPathOnHost
		}
	}
	st, err := os.Stat(resolvedDevicePath)
	if err != nil {
		return errors.Wrapf(err, "cannot stat %s", devicePath)
	}
	if st.IsDir() {
		found := false
		src := resolvedDevicePath
		dest := src
		var devmode string
		if len(devs) > 1 {
			if len(devs[1]) > 0 && devs[1][0] == '/' {
				dest = devs[1]
			} else {
				devmode = devs[1]
			}
		}
		if len(devs) > 2 {
			if devmode != "" {
				return errors.Wrapf(unix.EINVAL, "invalid device specification %s", devicePath)
			}
			devmode = devs[2]
		}

		// mount the internal devices recursively
		if err := filepath.Walk(resolvedDevicePath, func(dpath string, f os.FileInfo, e error) error {

			if f.Mode()&os.ModeDevice == os.ModeDevice {
				found = true
				device := fmt.Sprintf("%s:%s", dpath, filepath.Join(dest, strings.TrimPrefix(dpath, src)))
				if devmode != "" {
					device = fmt.Sprintf("%s:%s", device, devmode)
				}
				if err := addDevice(g, device); err != nil {
					return errors.Wrapf(err, "failed to add %s device", dpath)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if !found {
			return errors.Wrapf(unix.EINVAL, "no devices found in %s", devicePath)
		}
		return nil
	}

	return addDevice(g, strings.Join(append([]string{resolvedDevicePath}, devs[1:]...), ":"))
}

func addDevice(g *generate.Generator, device string) error {
	src, dst, permissions, err := ParseDevice(device)
	if err != nil {
		return err
	}
	dev, err := devices.DeviceFromPath(src, permissions)
	if err != nil {
		return errors.Wrapf(err, "%s is not a valid device", src)
	}
	dev.Path = dst
	linuxdev := spec.LinuxDevice{
		Path:     dev.Path,
		Type:     string(dev.Type),
		Major:    dev.Major,
		Minor:    dev.Minor,
		FileMode: &dev.FileMode,
		UID:      &dev.Uid,
		GID:      &dev.Gid,
	}
	g.AddDevice(linuxdev)
	g.AddLinuxResourcesDevice(true, string(dev.Type), &dev.Major, &dev.Minor, dev.Permissions)
	return nil
}

func (c *CreateConfig) addPrivilegedDevices(g *generate.Generator) error {
	hostDevices, err := devices.HostDevices()
	if err != nil {
		return err
	}
	g.ClearLinuxDevices()
	for _, d := range hostDevices {
		g.AddDevice(Device(d))
	}

	// Add resources device - need to clear the existing one first.
	g.Spec().Linux.Resources.Devices = nil
	g.AddLinuxResourcesDevice(true, "", nil, nil, "rwm")
	return nil
}

func (c *CreateConfig) createBlockIO() (*spec.LinuxBlockIO, error) {
	var ret *spec.LinuxBlockIO
	bio := &spec.LinuxBlockIO{}
	if c.Resources.BlkioWeight > 0 {
		ret = bio
		bio.Weight = &c.Resources.BlkioWeight
	}
	if len(c.Resources.BlkioWeightDevice) > 0 {
		var lwds []spec.LinuxWeightDevice
		ret = bio
		for _, i := range c.Resources.BlkioWeightDevice {
			wd, err := validateweightDevice(i)
			if err != nil {
				return ret, errors.Wrapf(err, "invalid values for blkio-weight-device")
			}
			wdStat, err := getStatFromPath(wd.path)
			if err != nil {
				return ret, errors.Wrapf(err, "error getting stat from path %q", wd.path)
			}
			lwd := spec.LinuxWeightDevice{
				Weight: &wd.weight,
			}
			lwd.Major = int64(unix.Major(wdStat.Rdev))
			lwd.Minor = int64(unix.Minor(wdStat.Rdev))
			lwds = append(lwds, lwd)
		}
		bio.WeightDevice = lwds
	}
	if len(c.Resources.DeviceReadBps) > 0 {
		ret = bio
		readBps, err := makeThrottleArray(c.Resources.DeviceReadBps, bps)
		if err != nil {
			return ret, err
		}
		bio.ThrottleReadBpsDevice = readBps
	}
	if len(c.Resources.DeviceWriteBps) > 0 {
		ret = bio
		writeBpds, err := makeThrottleArray(c.Resources.DeviceWriteBps, bps)
		if err != nil {
			return ret, err
		}
		bio.ThrottleWriteBpsDevice = writeBpds
	}
	if len(c.Resources.DeviceReadIOps) > 0 {
		ret = bio
		readIOps, err := makeThrottleArray(c.Resources.DeviceReadIOps, iops)
		if err != nil {
			return ret, err
		}
		bio.ThrottleReadIOPSDevice = readIOps
	}
	if len(c.Resources.DeviceWriteIOps) > 0 {
		ret = bio
		writeIOps, err := makeThrottleArray(c.Resources.DeviceWriteIOps, iops)
		if err != nil {
			return ret, err
		}
		bio.ThrottleWriteIOPSDevice = writeIOps
	}
	return ret, nil
}

func makeThrottleArray(throttleInput []string, rateType int) ([]spec.LinuxThrottleDevice, error) {
	var (
		ltds []spec.LinuxThrottleDevice
		t    *throttleDevice
		err  error
	)
	for _, i := range throttleInput {
		if rateType == bps {
			t, err = validateBpsDevice(i)
		} else {
			t, err = validateIOpsDevice(i)
		}
		if err != nil {
			return []spec.LinuxThrottleDevice{}, err
		}
		ltdStat, err := getStatFromPath(t.path)
		if err != nil {
			return ltds, errors.Wrapf(err, "error getting stat from path %q", t.path)
		}
		ltd := spec.LinuxThrottleDevice{
			Rate: t.rate,
		}
		ltd.Major = int64(unix.Major(ltdStat.Rdev))
		ltd.Minor = int64(unix.Minor(ltdStat.Rdev))
		ltds = append(ltds, ltd)
	}
	return ltds, nil
}

func getStatFromPath(path string) (unix.Stat_t, error) {
	s := unix.Stat_t{}
	err := unix.Stat(path, &s)
	return s, err
}
