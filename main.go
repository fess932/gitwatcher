package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	tokenFlag = flag.String("token", "", "GitHub token (required)")
	deployCmd = flag.String("deploy", "", "deploy command (sh -c, required)")
)

var (
	lastSHA string

	mu            sync.Mutex
	deployCtx     context.Context
	deployCancel  context.CancelFunc
	deployProcess *exec.Cmd
	retryDelay    = 10 * time.Second
)

func main() {
	flag.Parse()
	if *tokenFlag == "" || *deployCmd == "" {
		log.Fatal("flags -token and -deploy are required")
	}

	for {
		check()
		time.Sleep(5 * time.Second)
	}
}

func getCurrentBranch() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func getRepoOwnerAndName() (string, error) {
	out, err := exec.Command("git", "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return "", err
	}
	url := strings.TrimSpace(string(out))
	url = strings.TrimSuffix(url, ".git")
	url = strings.Replace(url, "git@github.com:", "", 1)
	url = strings.Replace(url, "https://github.com/", "", 1)
	return url, nil
}

func check() {
	branch, err := getCurrentBranch()
	if err != nil {
		log.Println("cannot get current branch:", err)
		return
	}

	repoURL, err := getRepoOwnerAndName()
	if err != nil {
		log.Println("cannot get repo owner/name:", err)
		return
	}

	apiURL := "https://api.github.com/repos/" + repoURL + "/commits/" + branch
	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("Authorization", "Bearer "+*tokenFlag)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Println("request error:", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Println("bad status:", resp.Status)
		return
	}

	var data struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Println("decode error:", err)
		return
	}

	currentSHA := strings.TrimSpace(data.SHA)
	if currentSHA == lastSHA {
		return
	}

	lastSHA = currentSHA
	log.Println("New commit detected:", lastSHA)

	go runDeploy()
}

func runDeploy() {
	mu.Lock()
	if deployCancel != nil {
		log.Println("Cancelling previous deploy...")
		deployCancel()
	}
	if deployProcess != nil && deployProcess.Process != nil {
		log.Println("Killing previous server process group...")
		pgid, err := syscall.Getpgid(deployProcess.Process.Pid)
		if err == nil {
			syscall.Kill(-pgid, syscall.SIGKILL) // Kill entire process group
		}
		deployProcess.Wait()
		time.Sleep(2 * time.Second) // Wait for port to be released
	}
	deployCtx, deployCancel = context.WithCancel(context.Background())
	mu.Unlock()

	for {
		select {
		case <-deployCtx.Done():
			log.Println("Deploy cancelled")
			return
		default:
			log.Println("Starting deploy...")

			if err := exec.CommandContext(deployCtx, "git", "pull").Run(); err != nil {
				log.Println("git pull failed:", err)
				time.Sleep(retryDelay)
				continue
			}

			cmd := exec.CommandContext(deployCtx, "sh", "-c", *deployCmd)
			cmd.Stdout = log.Writer()
			cmd.Stderr = log.Writer()
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

			mu.Lock()
			deployProcess = cmd
			mu.Unlock()

			if err := cmd.Run(); err != nil {
				log.Println("deploy command failed:", err)
				time.Sleep(retryDelay)
				continue
			}

			log.Println("Deploy finished successfully")
			return
		}
	}
}
