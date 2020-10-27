package storiface

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/lotus/extern/sector-storage/sealtasks"
)

type WorkerInfo struct {
	Hostname string

	TaskResourcesLk sync.Mutex                         // 添加的内容
	TaskResources   map[sealtasks.TaskType]*TaskConfig // 添加的内容
	Resources       WorkerResources
}

type WorkerResources struct {
	MemPhysical uint64
	MemSwap     uint64

	MemReserved uint64 // Used by system / other processes

	CPUs uint64 // Logical cores
	GPUs []string
}

type WorkerStats struct {
	Info WorkerInfo

	MemUsedMin uint64
	MemUsedMax uint64
	GpuUsed    bool
	CpuUse     uint64
}

type WorkerJob struct {
	ID     uint64
	Sector abi.SectorID
	Task   sealtasks.TaskType

	RunWait int // 0 - running, 1+ - assigned
	Start   time.Time
}

//===================添加的内容====================//
type TaskConfig struct {
	LimitCount int
	RunCount   int
}

type taskLimitConfig struct {
	AddPiece     int
	PreCommit1   int
	PreCommit2   int
	Commit1      int
	Commit2      int
	Fetch        int
	Finalize     int
	Unseal       int
	ReadUnsealed int
}

func NewTaskLimitConfig() map[sealtasks.TaskType]*TaskConfig {
	config := &taskLimitConfig{
		AddPiece:     1,
		PreCommit1:   1,
		PreCommit2:   1,
		Commit1:      1,
		Commit2:      1,
		Fetch:        1,
		Finalize:     1,
		Unseal:       1,
		ReadUnsealed: 1,
	}

	ability := ""
	if env, ok := os.LookupEnv("ABILITY"); ok {
		ability = strings.Replace(string(env), " ", "", -1) // 去除空格
		ability = strings.Replace(ability, "\n", "", -1)    // 去除换行符
	}
	splitArr := strings.Split(ability, ",")
	for _, part := range splitArr {
		if strings.Contains(part, "AP:") {
			splitPart := strings.Split(part, ":")
			if intCount, err := strconv.Atoi(splitPart[1]); err == nil {
				config.AddPiece = intCount
			}
		} else if strings.Contains(part, "PC1:") {
			splitPart := strings.Split(part, ":")
			if intCount, err := strconv.Atoi(splitPart[1]); err == nil {
				config.PreCommit1 = intCount
			}
		} else if strings.Contains(part, "PC2:") {
			splitPart := strings.Split(part, ":")
			if intCount, err := strconv.Atoi(splitPart[1]); err == nil {
				config.PreCommit2 = intCount
			}
		} else if strings.Contains(part, "C1:") {
			splitPart := strings.Split(part, ":")
			if intCount, err := strconv.Atoi(splitPart[1]); err == nil {
				config.Commit1 = intCount
			}
		} else if strings.Contains(part, "C2:") {
			splitPart := strings.Split(part, ":")
			if intCount, err := strconv.Atoi(splitPart[1]); err == nil {
				config.Commit2 = intCount
			}
		} else if strings.Contains(part, "GET:") {
			splitPart := strings.Split(part, ":")
			if intCount, err := strconv.Atoi(splitPart[1]); err == nil {
				config.Fetch = intCount
			}
		} else if strings.Contains(part, "FIN:") {
			splitPart := strings.Split(part, ":")
			if intCount, err := strconv.Atoi(splitPart[1]); err == nil {
				config.Finalize = intCount
			}
		} else if strings.Contains(part, "UNS:") {
			splitPart := strings.Split(part, ":")
			if intCount, err := strconv.Atoi(splitPart[1]); err == nil {
				config.Unseal = intCount
			}
		} else if strings.Contains(part, "RD:") {
			splitPart := strings.Split(part, ":")
			if intCount, err := strconv.Atoi(splitPart[1]); err == nil {
				config.ReadUnsealed = intCount
			}
		}
	}

	cfgResources := make(map[sealtasks.TaskType]*TaskConfig)

	if _, ok := cfgResources[sealtasks.TTAddPiece]; !ok {
		cfgResources[sealtasks.TTAddPiece] = &TaskConfig{
			LimitCount: config.AddPiece,
			RunCount:   0,
		}
	}

	if _, ok := cfgResources[sealtasks.TTPreCommit1]; !ok {
		cfgResources[sealtasks.TTPreCommit1] = &TaskConfig{
			LimitCount: config.PreCommit1,
			RunCount:   0,
		}
	}

	if _, ok := cfgResources[sealtasks.TTPreCommit2]; !ok {
		cfgResources[sealtasks.TTPreCommit2] = &TaskConfig{
			LimitCount: config.PreCommit2,
			RunCount:   0,
		}
	}

	if _, ok := cfgResources[sealtasks.TTCommit1]; !ok {
		cfgResources[sealtasks.TTCommit1] = &TaskConfig{
			LimitCount: config.Commit1,
			RunCount:   0,
		}
	}

	if _, ok := cfgResources[sealtasks.TTCommit2]; !ok {
		cfgResources[sealtasks.TTCommit2] = &TaskConfig{
			LimitCount: config.Commit2,
			RunCount:   0,
		}
	}

	if _, ok := cfgResources[sealtasks.TTFetch]; !ok {
		cfgResources[sealtasks.TTFetch] = &TaskConfig{
			LimitCount: config.Fetch,
			RunCount:   0,
		}
	}

	if _, ok := cfgResources[sealtasks.TTFinalize]; !ok {
		cfgResources[sealtasks.TTFinalize] = &TaskConfig{
			LimitCount: config.Finalize,
			RunCount:   0,
		}
	}

	if _, ok := cfgResources[sealtasks.TTUnseal]; !ok {
		cfgResources[sealtasks.TTUnseal] = &TaskConfig{
			LimitCount: config.Unseal,
			RunCount:   0,
		}
	}

	if _, ok := cfgResources[sealtasks.TTReadUnsealed]; !ok {
		cfgResources[sealtasks.TTReadUnsealed] = &TaskConfig{
			LimitCount: config.ReadUnsealed,
			RunCount:   0,
		}
	}

	return cfgResources
}

//===================添加的内容====================//
