// Command executorserver will starts a http server that receives command to run
// programs inside a sandbox.
package main

import (
	"flag"
	"io/ioutil"
	"log"
	"os"
	"sync/atomic"
	"syscall"

	"github.com/criyle/go-judge/pkg/envexec"
	"github.com/criyle/go-judge/pkg/pool"
	"github.com/criyle/go-sandbox/container"
	"github.com/criyle/go-sandbox/pkg/cgroup"
	"github.com/criyle/go-sandbox/pkg/forkexec"
	"github.com/criyle/go-sandbox/pkg/mount"
	"github.com/gin-gonic/gin"
)

var (
	addr       = flag.String("http", ":5050", "specifies the http binding address")
	parallism  = flag.Int("parallism", 4, "control the # of concurrency execution")
	tmpFsParam = flag.String("tmpfs", "size=8m,nr_inodes=4k", "tmpfs mount data")
	dir        = flag.String("dir", "", "specifies direcotry to store file upload / download (in memory by default)")
	silent     = flag.Bool("silent", false, "do not print logs")
	netShare   = flag.Bool("net", false, "do not unshare net namespace with host")

	envPool    envexec.EnvironmentPool
	cgroupPool envexec.CgroupPool

	fs fileStore

	printLog = log.Println
)

func init() {
	container.Init()
}

func main() {
	flag.Parse()

	if *dir == "" {
		fs = newFileMemoryStore()
	} else {
		os.MkdirAll(*dir, 0755)
		fs = newFileLocalStore(*dir)
	}
	if *silent {
		printLog = func(v ...interface{}) {}
	}

	root, err := ioutil.TempDir("", "dm")
	if err != nil {
		panic(err)
	}
	printLog("Created tmp dir for container root at:", root)

	mb := mount.NewBuilder().
		// basic exec and lib
		WithBind("/bin", "bin", true).
		WithBind("/lib", "lib", true).
		WithBind("/lib64", "lib64", true).
		WithBind("/usr", "usr", true).
		// java wants /proc/self/exe as it need relative path for lib
		// however, /proc gives interface like /proc/1/fd/3 ..
		// it is fine since open that file will be a EPERM
		// changing the fs uid and gid would be a good idea
		WithProc().
		// some compiler have multiple version
		WithBind("/etc/alternatives", "etc/alternatives", true).
		// fpc wants /etc/fpc.cfg
		WithBind("/etc/fpc.cfg", "etc/fpc.cfg", true).
		// go wants /dev/null
		WithBind("/dev/null", "dev/null", false).
		// ghc wants /var/lib/ghc
		WithBind("/var/lib/ghc", "var/lib/ghc", true).
		// work dir
		WithTmpfs("w", *tmpFsParam).
		// tmp dir
		WithTmpfs("tmp", *tmpFsParam)
	m, err := mb.Build(true)
	if err != nil {
		panic(err)
	}
	printLog("Created default container mount at:", mb)

	unshareFlags := uintptr(forkexec.UnshareFlags)
	if *netShare {
		unshareFlags ^= syscall.CLONE_NEWNET
	}

	b := &container.Builder{
		Root:          root,
		Mounts:        m,
		CredGenerator: newCredGen(),
		Stderr:        true,
		CloneFlags:    unshareFlags,
	}
	cgb, err := cgroup.NewBuilder("executorserver").WithCPUAcct().WithMemory().WithPids().FilterByEnv()
	if err != nil {
		panic(err)
	}
	printLog("Created cgroup builder with:", cgb)

	envPool = pool.NewEnvPool(b)
	cgroupPool = pool.NewFakeCgroupPool(cgb)

	printLog("Starting worker with parallism", *parallism)
	startWorkers()

	var r *gin.Engine
	if *silent {
		gin.SetMode(gin.ReleaseMode)
		r = gin.New()
	} else {
		r = gin.Default()
	}
	r.GET("/file", fileGet)
	r.POST("/file", filePost)
	r.GET("/file/:fid", fileIDGet)
	r.DELETE("/file/:fid", fileIDDelete)
	r.POST("/run", handleRun)

	printLog("Starting http server at", *addr)
	r.Run(*addr)
}

type credGen struct {
	cur uint32
}

func newCredGen() *credGen {
	return &credGen{cur: 10000}
}

func (c *credGen) Get() syscall.Credential {
	n := atomic.AddUint32(&c.cur, 1)
	return syscall.Credential{
		Uid: n,
		Gid: n,
	}
}
