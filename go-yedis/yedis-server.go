package main

import (
	"Monica/go-yedis/command/db"
	"Monica/go-yedis/command/hash"
	"Monica/go-yedis/command/list"
	"Monica/go-yedis/command/sds"
	"Monica/go-yedis/command/set"
	"Monica/go-yedis/core"
	"Monica/go-yedis/utils"
	"flag"
	"fmt"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"
	"log"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

const (
	//默认配置文件路径
	defaultConfigPath = "yedis.conf"

	//默认数据库的键值对初始容量
	defaultDbDictCapacity = 100

	//aof功能开闭状态
	REDIS_AOF_OFF = 0
	REDIS_AOF_ON = 1
)

//创建服务端实例
var yedis = new(core.YedisServer)

func main() {

	//获取用户输入的参数
	cmdArgs := os.Args

	//第一个参数是文件路径， 读取配置
	var configPath = defaultConfigPath
	if len(cmdArgs) > 1 && strings.LastIndex(cmdArgs[1], ".conf") != -1 {
		fmt.Println("读取配置")
		configPath = cmdArgs[1]
	} else if len(cmdArgs) == 2 {
		utils.BaseHelp(cmdArgs[1])
	}

	//获取配置
	netConfig, dbConfig, aofConfig := utils.ReadConfig(configPath)
	fmt.Println("初始化yedis.conf网络参数", netConfig)
	fmt.Println("初始化yedis.conf数据库参数", dbConfig)
	fmt.Println("初始化yedis.confAOF持久化参数", aofConfig)

	//读取命令行输入的ip和端口,并将命令行获取的值回写
	var netBind = flag.String("ip", netConfig.NetBind, "redis服务端IP")
	var netPort = flag.String("port", netConfig.NetPort, "redis服务端PORT")
	flag.Parse()
	netConfig.NetBind = *netBind
	netConfig.NetPort = *netPort

	host := *netBind + ":" + *netPort
	log.Println("Redis实例化地址：" + host)

	//监听退出事件做相应处理
	utils.ExitHandler()

	//初始化服务端实例
	initServer(netConfig, dbConfig, aofConfig, configPath)

	// 事件循环有点复杂了，先直接go xx试下

	eventId := core.AeCreateTimeEvent(yedis, 1, core.ServerCron, nil, nil)
	if eventId == core.AE_ERR {
		log.Println("Can't create the serverCron time event.")
		os.Exit(core.AE_ERR)
	}

	//初始化网络监听并延时关闭
	//redis3.0代码：https://github.com/huangz1990/redis-3.0-annotated/blob/8e60a75884e75503fb8be1a322406f21fb455f67/src/redis.c#L1981
	netListener, err := net.Listen("tcp", host)
	if err != nil {
		log.Println("net listen err:", err)
	}
	defer netListener.Close()

	//开始运行事件处理器，生命周期为Redis服务器创建到销毁
	go core.AeMain(yedis)

	//如果aof功能打开了，则打开或创建一个aof文件
	if yedis.AofState == REDIS_AOF_ON {
		// 以只写的模式，打开文件
		f, err := os.OpenFile(yedis.AofFileName, os.O_WRONLY|syscall.O_CREAT, 0644)
		if err != nil {
			log.Println(err)
			return
		}
		yedis.AofFd = f
	}

	//将磁盘数据加载到内存中
	loadDataFromDisk()

	//循环监听新连接，将新连接放入go协程中处理
	for {
		conn, err := netListener.Accept()
		if err != nil {
			continue
		}
		go handle(conn)
	}


}

//处理连接请求
func handle(conn net.Conn) {
	//通过服务器给新的请求创建一个连接
	c := yedis.CreateClient()
	for {
		//从连接中读取命令，并写入到Client对象中
		err := c.ReadCommandFromClient(conn)
		if err != nil {
			log.Println("ReadCommandFromClient err", err)
			return
		}

		//解析命令到Client的Argv中
		err = c.ProcessCommandInfo()
		if err != nil {
			log.Println("ProcessCommandInfo err", err)
			continue
		}

		//执行命令
		yedis.ExecuteCommand(c)

		//响应客户端
		response2Client(conn, c)

	}
}

// 响应返回给客户端
func response2Client(conn net.Conn, c *core.YedisClients) {
	_, err := conn.Write([]byte(c.Reply))
	//log.Println("响应的response字节数为：", responseSize)
	utils.ErrorVerify("消息响应客户端失败", err, false)
}

//初始化服务端实例, 将yedis.conf配置写入server实例
//redis3.0代码地址：https://github.com/huangz1990/redis-3.0-annotated/blob/8e60a75884e75503fb8be1a322406f21fb455f67/src/redis.c#L3952
func initServer(netConfig utils.NetConfig, dbConfig utils.DbConfig, aofConfig utils.AofConfig, configPath string) {
	//1. 写入基础配置
	yedis.Pid = os.Getpid()            //获取进程ID
	yedis.ConfigFile = configPath      //配置文件绝对路径
	yedis.DbNum = dbConfig.DbDatabases //配置db数量
	yedis.Hz = dbConfig.Hz             //配置任务执行频率
	yedis.MaxClients = netConfig.NetMaxclients //客户端连接的最大数
	yedis.El = core.AeCreateEventLoop(yedis.MaxClients + 96) //创建时间循环并赋值，暂时写死这个值
	yedis.ActiveExpireEnabled = 1      //打开过期键的清除策略
	initDb()                           //初始化server中的16个数据库

	//2. 网络配置
	yedis.BindAddr = netConfig.NetBind //配置绑定IP地址
	yedis.Port = netConfig.NetPort     //配置端口号

	//3. RDB persistence持久化
	yedis.Dirty = 1                           //存储上次数据变动前的长度
	yedis.RdbFileName = dbConfig.DbDbfilename //rdb文件名
	yedis.RdbCompression = core.DISABLE       //TODO 是否对rdb使用压缩
	yedis.SaveTime = dbConfig.DbSavetime      //指定在多长时间内，有多少次更新操作，就将数据同步到数据文件，默认：300秒内10次更新操作就同步数据到文件
	yedis.SaveNumber = dbConfig.DbSavenumber  //

	//4. AOF persistence持久化
	if aofConfig.AofAppendonly == "no" { //配置是否开启aof：number
		yedis.AofState = REDIS_AOF_OFF
	} else {
		yedis.AofState = REDIS_AOF_ON
	}
	yedis.AofEnabled = aofConfig.AofAppendonly        //配置是否开启aof：字符串
	yedis.AofFileName = aofConfig.AofAppendfilename //配置aof文件名
	switch aofConfig.AofAppendfsync { //配置同步文件的策略
	case "no":
		yedis.AofFsync = core.AOF_FSYNC_NO
	case "everysec":
		yedis.AofFsync = core.AOF_FSYNC_EVERYSEC
	case "always":
		yedis.AofFsync = core.AOF_FSYNC_ALWAYS
	}

	//5. 仅用于统计使用的字段，仅取部分
	yedis.StatStartTime = time.Now().UnixNano() / 1000000 //记录服务启动时间
	yedis.StatNumCommands = int16(len(yedis.Commands))    //支持的命令数量
	yedis.StatNumConnections = int16(0)                   //当前连接数量
	yedis.StatExpiredKeys = int64(0)                      //当前失效key的数量

	//6. 系统硬件信息
	memInfo, err := mem.VirtualMemory() //获取机器内存信息
	utils.ErrorVerify("获取机器内存信息失败", err, true)
	yedis.SystemAllMemorySize = memInfo.Total     //机器总内存大小 单位：b
	yedis.SystemAvailableSize = memInfo.Available //机器可用内存大小 单位：b
	yedis.SystemUsedSize = memInfo.Used           //机器已用内存大小 单位：b
	yedis.SystemUsedPercent = memInfo.UsedPercent //机器已用内存百分比

	percent, err := cpu.Percent(time.Second, false)
	utils.ErrorVerify("获取机器CPU信息失败", err, true)
	yedis.SystemCpuPercent = percent[0] //CPU使用百分比情况

	//初始化服务支持命令
	getCommand := &core.YedisCommand{Name: "get", CommandProc: sds.GetCommand, Arity: 2}
	setCommand := &core.YedisCommand{Name: "set", CommandProc: sds.SetCommand, Arity: 3}
	strlenCommand := &core.YedisCommand{Name: "strlen", CommandProc: sds.StrlenCommand, Arity: 2}
	appendCommand := &core.YedisCommand{Name: "append", CommandProc: sds.AppendCommand, Arity: 3}
	getrangeCommand := &core.YedisCommand{Name: "getrange", CommandProc: sds.GetrangeCommand, Arity: 4}
	mgetCommand := &core.YedisCommand{Name: "mget", CommandProc: sds.MgetCommand, Arity: 0}

	incrCommand := &core.YedisCommand{Name: "incr", CommandProc: sds.IncrCommand, Arity: 2}
	incrbyCommand := &core.YedisCommand{Name: "incrby", CommandProc: sds.IncrbyCommand, Arity: 3}
	decrCommand := &core.YedisCommand{Name: "decr", CommandProc: sds.DecrCommand, Arity: 2}
	decrbyCommand := &core.YedisCommand{Name: "decrby", CommandProc: sds.DecrbyCommand, Arity: 3}

	pexpireCommand := &core.YedisCommand{Name: "pexpire", CommandProc: sds.PexpireCommand, Arity: 3}
	pexpireatCommand := &core.YedisCommand{Name: "pexpireat", CommandProc: sds.PexpireatCommand, Arity: 3}
	expireCommand := &core.YedisCommand{Name: "expire", CommandProc: sds.ExpireCommand, Arity: 3}
	expireatCommand := &core.YedisCommand{Name: "expireat", CommandProc: sds.ExpireatCommand, Arity: 3}

	pttlCommand := &core.YedisCommand{Name: "pttl", CommandProc: sds.PttlCommand, Arity: 2}
	ttlCommand := &core.YedisCommand{Name: "ttl", CommandProc: sds.TtlCommand, Arity: 2}

	infoCommand := &core.YedisCommand{Name: "info", CommandProc: sds.InfoCommand, Arity: 1}
	selectCommand := &core.YedisCommand{Name: "select", CommandProc: db.SelectCommand, Arity: 2}
	keysCommand := &core.YedisCommand{Name: "keys", CommandProc: db.KeysCommand, Arity: 2}

	lpushCommand := &core.YedisCommand{Name: "lpush", CommandProc: list.LpushCommand, Arity: 0}
	rpushCommand := &core.YedisCommand{Name: "rpush", CommandProc: list.RpushCommand, Arity: 0}
	llenCommand := &core.YedisCommand{Name: "llen", CommandProc: list.LlenCommand, Arity: 2}
	lindexCommand := &core.YedisCommand{Name: "lindex", CommandProc: list.LindexCommand, Arity: 3}
	lsetCommand := &core.YedisCommand{Name: "lset", CommandProc: list.LsetCommand, Arity: 4}
	linsertCommand := &core.YedisCommand{Name: "linsert", CommandProc: list.LinsertCommand, Arity: 5}
	lrangeCommand := &core.YedisCommand{Name: "lrange", CommandProc: list.LrangeCommand, Arity: 4}
	lpopCommand := &core.YedisCommand{Name: "lpop", CommandProc: list.LpopCommand, Arity: 2}
	rpopCommand := &core.YedisCommand{Name: "rpop", CommandProc: list.RpopCommand, Arity: 2}
	lremCommand := &core.YedisCommand{Name: "lrem", CommandProc: list.LremCommand, Arity: 4}

	hsetCommand := &core.YedisCommand{Name: "hset", CommandProc: hash.HsetCommand, Arity: 4}
	hgetCommand := &core.YedisCommand{Name: "hget", CommandProc: hash.HgetCommand, Arity: 3}
	hlenCommand := &core.YedisCommand{Name: "hlen", CommandProc: hash.HlenCommand, Arity: 2}
	hgetallCommand := &core.YedisCommand{Name: "hgetall", CommandProc: hash.HgetallCommand, Arity: 2}
	hexistsCommand := &core.YedisCommand{Name: "hexists", CommandProc: hash.HexistsCommand, Arity: 3}
	hdelCommand := &core.YedisCommand{Name: "hdel", CommandProc: hash.HdelCommand, Arity: 0}

	saddCommand := &core.YedisCommand{Name: "sadd", CommandProc: set.SaddCommand, Arity: 0}
	scardCommand := &core.YedisCommand{Name: "scard", CommandProc: set.ScardCommand, Arity: 2}
	sismemberCommand := &core.YedisCommand{Name: "scard", CommandProc: set.SismemberCommand, Arity: 3}
	smembersCommand := &core.YedisCommand{Name: "scard", CommandProc: set.SmembersCommand, Arity: 2}
	spopCommand := &core.YedisCommand{Name: "scard", CommandProc: set.SpopCommand, Arity: 2}
	srandmemberCommand := &core.YedisCommand{Name: "scard", CommandProc: set.SrandmemberCommand, Arity: 2}
	sremCommand := &core.YedisCommand{Name: "scard", CommandProc: set.SremCommand, Arity: 3}


	yedis.Commands = map[string]*core.YedisCommand{
		"get":      getCommand,
		"set":      setCommand,
		"strlen":   strlenCommand,
		"append":   appendCommand,
		"getrange": getrangeCommand,
		"mget":     mgetCommand,

		"incr":   incrCommand,
		"incrby": incrbyCommand,
		"decr":   decrCommand,
		"decrby": decrbyCommand,

		"pexpire": pexpireCommand,
		"pexpireat": pexpireatCommand,
		"expire": expireCommand,
		"expireat": expireatCommand,

		"pttl": pttlCommand,
		"ttl": ttlCommand,

		"info": infoCommand,
		"select": selectCommand,
		"keys": keysCommand,

		"lpush": lpushCommand,
		"rpush": rpushCommand,
		"llen": llenCommand,
		"lindex": lindexCommand,
		"lset": lsetCommand,
		"linsert": linsertCommand,
		"lrange": lrangeCommand,
		"lpop": lpopCommand,
		"rpop": rpopCommand,
		"lrem": lremCommand,

		"hset": hsetCommand,
		"hget": hgetCommand,
		"hlen": hlenCommand,
		"hgetall": hgetallCommand,
		"hexists": hexistsCommand,
		"hdel": hdelCommand,

		"sadd": saddCommand,
		"scard": scardCommand,
		"sismember": sismemberCommand,
		"smembers": smembersCommand,
		"spop": spopCommand,
		"srandmember": srandmemberCommand,
		"srem": sremCommand,
	}

}

//初始化数据库
func initDb() {
	//创建一个储存数据库对象的切片
	yedis.ServerDb = make([]*core.YedisDb, yedis.DbNum)
	for i := 0; i < yedis.DbNum; i++ {
		//创建YedisDb数据库对象并对其中数据库ID和键值对字段赋值
		//键值对容量暂写死为200
		yedis.ServerDb[i] = new(core.YedisDb)
		yedis.ServerDb[i].ID = int8(i)
		yedis.ServerDb[i].Dict = make(core.Dict, defaultDbDictCapacity)
		yedis.ServerDb[i].Expires = make(core.ExpireDict, defaultDbDictCapacity)
		yedis.ServerDb[i].AvgTTL = 0

	}
}

//从磁盘中加载数据到内存中
//Redis实现：https://github.com/huangz1990/redis-3.0-annotated/blob/8e60a75884e75503fb8be1a322406f21fb455f67/src/redis.c#L3887
func loadDataFromDisk() {
	//记录开始时间，单位纳秒
	start := utils.CurrentTimeNano()

	//如果aof打开了优先使用aof，因为aof的持久化比rdb更全面一点
	if yedis.AofState == REDIS_AOF_ON {
		//载入AOF操作
		if core.LoadAppendOnlyFile(yedis) == core.REDIS_OK {
			//输出载入耗时
			fmt.Printf("DB loaded from append only file: %.3f seconds", float64(utils.CurrentTimeNano()-start)/1000000)
		}else {
			//TODO 载入RDB操作
			if core.RdbLoad(yedis.RdbFileName) == core.REDIS_OK {
				fmt.Printf("DB loaded from disk: %.3f seconds", float64(utils.CurrentTimeNano()-start)/1000000)
			}
		}
	}
}