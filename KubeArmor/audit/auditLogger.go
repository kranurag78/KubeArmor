package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	kl "github.com/accuknox/KubeArmor/KubeArmor/common"
	kg "github.com/accuknox/KubeArmor/KubeArmor/log"
	tp "github.com/accuknox/KubeArmor/KubeArmor/types"
)

// ================== //
// == Audit Logger == //
// ================== //

// StopChan Channel
var StopChan chan struct{}

// init Function
func init() {
	StopChan = make(chan struct{})
}

// AuditLogger Structure
type AuditLogger struct {
	// logging type
	logType string

	// logging path
	logFile string

	// hostname for logging
	HostName string

	// container id => cotnainer
	Containers     map[string]tp.Container
	ContainersLock *sync.Mutex

	// container id => pid
	ActivePidMap     map[string]tp.PidMap
	ActivePidMapLock *sync.Mutex

	// COS flag
	isCOS bool
}

// NewAuditLogger Function
func NewAuditLogger(logOption, hostName string, containers map[string]tp.Container, containersLock *sync.Mutex, activePidMap map[string]tp.PidMap, activePidMapLock *sync.Mutex) *AuditLogger {
	al := &AuditLogger{}

	if kl.IsK8sLocal() {
		// create a test directory
		kl.GetCommandWithoutOutput("/bin/mkdir", []string{"-p", "/KubeArmor/audit"})
	}

	if strings.Contains(logOption, "file:") {
		args := strings.Split(logOption, ":")

		al.logType = args[0]
		al.logFile = args[1]

		// create log file
		kl.GetCommandWithoutOutput("/bin/touch", []string{al.logFile})
	} else {
		if logOption != "stdout" {
			kg.Printf("Use the default logging option (stdout) since %s is not a supported logging option", logOption)
		}

		al.logType = "stdout"
		al.logFile = ""
	}

	al.HostName = hostName

	al.Containers = containers
	al.ContainersLock = containersLock

	al.ActivePidMap = activePidMap
	al.ActivePidMapLock = activePidMapLock

	al.isCOS = false

	return al
}

// InitAuditLogger Function
func (al *AuditLogger) InitAuditLogger(homeDir string) error {
	// check if COS
	if b, err := ioutil.ReadFile("/media/root/etc/os-release"); err == nil {
		s := string(b)
		if strings.Contains(s, "Container-Optimized OS") {
			al.isCOS = true

			kg.Printf("Trying to create a symbolic link to get audit logs")

			for {
				// create symbolic link
				if err := exec.Command(homeDir + "/GKE/create_symbolic_link.sh").Run(); err == nil {
					break
				} else {
					// wait until cos-auditd is ready
					time.Sleep(time.Second * 1)
				}
			}

			kg.Printf("Created a symbolic link to get audit logs")
		}
	} else { // Otherwise
		// create audit log
		kl.GetCommandWithoutOutput("/bin/touch", []string{"/KubeArmor/audit/audit.log"})

		// load audit rules
		kl.GetCommandWithoutOutput("/sbin/auditctl", []string{"-R", "/etc/audit/audit.rules", ">", "/dev/null"})
	}

	return nil
}

// DestroyAuditLogger Function
func (al *AuditLogger) DestroyAuditLogger() {
	close(StopChan)

	if kl.IsK8sLocal() {
		// remove the test directory
		kl.GetCommandWithoutOutput("/bin/rm", []string{"-rf", "/KubeArmor"})
	}

	if !al.isCOS {
		// stop the Auditd daemon
		kl.GetCommandWithoutOutput("/usr/bin/pkill", []string{"-9", "auditd"})
	}
}

// ================ //
// == Audit Logs == //
// ================ //

// GetContainerInfoFromHostPid Function
func (al *AuditLogger) GetContainerInfoFromHostPid(hostPidInt int) (string, string) {
	hostPid := uint32(hostPidInt)

	al.ActivePidMapLock.Lock()
	defer al.ActivePidMapLock.Unlock()

	containerID := ""

	for id, v := range al.ActivePidMap {
		for pid := range v {
			if hostPid == pid {
				containerID = id
				break
			}
		}

		if containerID != "" {
			break
		}
	}

	al.ContainersLock.Lock()
	defer al.ContainersLock.Unlock()

	if containerID != "" {
		if val, ok := al.Containers[containerID]; ok {
			return containerID, val.ContainerName
		}
		return containerID, ""
	}

	return "", al.HostName
}

// MonitorAuditLogs Function
func (al *AuditLogger) MonitorAuditLogs() {
	if !al.isCOS {
		// start auditd
		kl.GetCommandWithoutOutput("/sbin/auditd", []string{})
	}

	// monitor audit logs
	al.MonitorGenericAuditLogs()
}

// MonitorGenericAuditLogs Function
func (al *AuditLogger) MonitorGenericAuditLogs() {
	logFile := "/KubeArmor/audit/audit.log"

	if kl.IsK8sLocal() {
		logFile = "/var/log/audit/audit.log"
	}

	// monitor audit logs
	cmd := exec.Command("/usr/bin/tail", "-f", logFile)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		kg.Err(err.Error())
		return
	}

	if err := cmd.Start(); err != nil {
		kg.Err(err.Error())
		return
	}

	r := bufio.NewReader(stdout)

	for {
		select {
		case <-StopChan:
			stdout.Close()
			return

		default:
			lineBytes, _, err := r.ReadLine()
			if err != nil {
				continue
			}
			line := string(lineBytes)
			words := []string{}

			if al.isCOS {
				line = strings.Replace(line, "\\\"", "\"", -1)
			}

			// == //

			requiredKeywords := []string{
				"AVC",
				// "SYSCALL",
			}

			skip := true

			for _, keyword := range requiredKeywords {
				if strings.Contains(line, keyword) {
					skip = false
				}
			}

			if skip {
				continue
			}

			excludedKeywords := []string{
				"apparmor=\"STATUS\"",
				// "success=yes",
			}

			skip = false

			for _, keyword := range excludedKeywords {
				if strings.Contains(line, keyword) {
					skip = true
				}
			}

			if skip {
				continue
			}

			// == //

			if al.isCOS {
				tempWords := strings.Split(line, ",")

				for _, tempWord := range tempWords {
					if strings.Contains(tempWord, "\"MESSAGE\":") {
						message := strings.Split(tempWord, ":")
						innerWords := strings.Split(message[1], " ")
						words = innerWords[1:]
					}
				}
			} else {
				tempWords := strings.Split(line, " ")
				words = tempWords[2:]
			}

			// == //

			hostPid := 0
			source := ""
			operation := ""
			resource := ""
			action := ""

			for _, keyVal := range words {
				if strings.HasPrefix(keyVal, "pid=") {
					value := strings.Split(keyVal, "=")
					hostPid, _ = strconv.Atoi(value[1])
				} else if strings.HasPrefix(keyVal, "comm=") {
					value := strings.Split(keyVal, "=")
					source = strings.Replace(value[1], "\"", "", -1)
				} else if strings.HasPrefix(keyVal, "operation=") {
					value := strings.Split(keyVal, "=")
					operation = strings.Replace(value[1], "\"", "", -1)
				} else if strings.HasPrefix(keyVal, "name=") {
					value := strings.Split(keyVal, "=")
					resource = strings.Replace(value[1], "\"", "", -1)
				} else if strings.HasPrefix(keyVal, "apparmor=") {
					value := strings.Split(keyVal, "=")
					if value[1] == "\"DENIED\"" {
						action = "Block"
					} else {
						action = strings.Replace(value[1], "\"", "", -1)
					}
				}
			}

			// == //

			auditLog := tp.AuditLog{}
			auditLog.UpdatedTime = kl.GetDateTimeNow()

			auditLog.HostName = al.HostName
			auditLog.ContainerID, auditLog.ContainerName = al.GetContainerInfoFromHostPid(hostPid)

			auditLog.HostPID = hostPid
			auditLog.Source = source
			auditLog.Operation = operation
			auditLog.Resource = resource
			auditLog.Action = action

			// == //

			if al.logType == "file" {
				auditLog.Raw = strings.Join(words[:], " ")

				arr, _ := json.Marshal(auditLog)
				kl.StrToFile(string(arr), al.logFile)
			} else { // stdout
				// no auditLog.Raw

				arr, _ := json.Marshal(auditLog)
				fmt.Println(string(arr))
			}
		}
	}
}
