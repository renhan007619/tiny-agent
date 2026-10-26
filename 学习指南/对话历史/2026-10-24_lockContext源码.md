# 单实例 Redis 锁轮询实现

| 项目 | 内容 |
| --- | --- |
| 会话 | `session-c830032a-ac0d-4429-845c-59a4f43477cc` |
| 创建时间 | 2026-10-24 17:08 |
| 模型 | `tencent/deepseek/deepseek-flash` |
| 工作目录 | `D:\workspace\tiny-agent` |
| 预设 | `standard` |
| 规模 | 2 轮 · 41 事件 |
| 用量 | 输入 23,867 · 输出 8,480 · 缓存命中 57,600 · 合计 89,947 tokens（3 次模型调用，按事件累计） |
| 导出时间 | 2026-10-25 16:27 |

## Turn 1

### 👤 用户

func (m *Mutex) lockContext(ctx context.Context, tries int) error {
	if ctx == nil {
		ctx = context.Background()
	}

	value, err := m.genValueFunc()          // ① 归属权凭据（随机 base64）
	if err != nil {
		return err
	}

	var timer *time.Timer
	for i := range tries {                  // ② 按"次数"循环
		if i != 0 {                         // ③ 第一次不等待
			if timer == nil {
				timer = time.NewTimer(m.delayFunc(i))
			} else {
				timer.Reset(m.delayFunc(i))
			}

			select {
			case <-ctx.Done():
				timer.Stop()                // ④ 取消时显式 Stop
				return ErrFailed
			case <-timer.C:
			}
		}

		start := time.Now()

		n, err := func() (int, error) {     // ⑤ 一轮尝试 = 所有节点并发
			ctx, cancel := context.WithTimeout(ctx, time.Duration(int64(float64(m.expiry)*m.timeoutFactor)))
			defer cancel()
			return m.actOnPoolsAsync(func(pool redis.Pool) (bool, error) {
				return m.acquire(ctx, pool, value)
			})
		}()

		now := time.Now()
		until := now.Add(m.expiry - now.Sub(start) - time.Duration(int64(float64(m.expiry)*m.driftFactor))) // ⑥
		if n >= m.quorum && now.Before(until) {
			m.value = value
			m.until = until
			return nil
		}
		_, _ = func() (int, error) {        // ⑦ 失败回滚
			ctx, cancel := context.WithTimeout(ctx, time.Duration(int64(float64(m.expiry)*m.timeoutFactor)))
			defer cancel()
			return m.actOnPoolsAsync(func(pool redis.Pool) (bool, error) {
				return m.release(ctx, pool, value)
			})
		}()
		if i == tries-1 && err != nil {     // ⑧
			return err
		}
	}

	return ErrFailed
}

这个主动轮序的代码我看是红锁吧，你能不能就先给我最基础的单实例redis的主动轮询代码

<details>
<summary>🧠 思考过程</summary>

用户贴了一段 redsync（Go Redis 分布式锁库）的 lockContext 代码，问这是不是红锁，然后要求给最基础的单实例 Redis 主动轮询锁代码。

用户偏好：代码先展示不落盘？"给我代码" = 只展示不落盘。这里是要代码，直接在对话中输出。用户要求 Go 语言。用户偏好由浅入深、四维度讲解可选。这里是代码需求，直接给即可。

先回答：这段确实来自 redsync（红锁 = Redlock，多实例 quorum），并发对所有 pool 执行 acquire，quorum 判定。然后给单实例版本的最基础主动轮询代码。

要注意用户说"主动轮询"——即循环重试获取锁。给最基础的：

- SET key value NX PX ttl
- 轮询间隔
- 释放用 Lua 比对 value 再 DEL
- ctx 取消

不要落盘文件（用户偏好：给代码只展示）。也不要写验证代码去跑（排错偏好）。这里不是排错，但也不需要跑。

代码要简洁、有注释，按用户偏好分块标【逐行理解/理解/略读】？那是改文件时。这里可以直接给代码块。

注意 Go 版本：for i := range tries 是 Go 1.22+。

写单实例版：

```go
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	lockKey   = "lock:order:123"
	lockTTL   = 5 * time.Second  // 锁的租约
	waitStep  = 100 * time.Millisecond // 轮询间隔
)

var (
	ErrLockFailed = errors.New("lock failed: retries exhausted")
	ErrLockLost   = errors.New("lock lost")
)

// releaseScript: 只有 value 匹配才删除，避免误删别人的锁
var releaseScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("del", KEYS[1])
end
return 0
`)

func genValue() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
```

然后 Lock：

```go
func Lock(ctx context.Context, rdb *redis.Client, tries int) (string, error) {
	value, err := genValue()
	if err != nil {
		return "", err
	}
	ttl := lockTTL

	for i := 0; i < tries; i++ {
		if i != 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(waitStep):
			}
		}
		// 单实例：一次 SET NX PX 就够，没有 quorum
		ok, err := rdb.SetNX(ctx, lockKey, value, ttl).Result()
		if err != nil {
			if i == tries-1 {
				return "", fmt.Errorf("redis setnx: %w", err)
			}
			continue // 网络抖动，下一轮再试
		}
		if ok {
			return value, nil
		}
		// 没抢到，下一轮
	}
	return "", ErrLockFailed
}
```

Unlock:

```go
func Unlock(ctx context.Context, rdb *redis.Client, value string) error {
	n, err := releaseScript.Run(ctx, rdb, []string{lockKey}, value).Int64()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLockLost // 锁已过期或不是自己的
	}
	return nil
}
```

注意：redis.NewScript 的 lua 用双引号 "get" 而不是单引号，避免 Go 字符串问题——其实用反引号 raw string 就没问题，但 redis.call("get") 双引号更稳妥一致。

要不要加续期（watchdog）？最基础版不给，提一句作为后续。

对于 ctx：注意用 rdb.SetNX(ctx, ...) 时循环第一次不等待，符合原代码语义。

还有一个细节：单实例下没有 `until`/drift 计算——因为单次操作，过期时间就是 TTL，不需要扣减漂移。可以点一句：原代码 ⑥ 那行（until = expiry - 耗时 - drift）只在红锁有意义，因为要在所有节点上校验"有效期内仍然多数派持有"。

用户四维度要求是针对算法答疑的。这里是工程代码讲解，但我可以按"值得记的点"标注。保持简洁。

回答结构：
1. 先回答：是红锁（Redlock），依据三处：actOnPoolsAsync 多 pool 并发、n >= m.quorum、until 扣掉耗时和 drift。
2. 单实例最基础主动轮询代码（分三块：加锁/解锁/使用示例）。
3. 与红锁的对应关系 + 关键取舍点（值得记 vs 了解即可）。

不需要工具调用。直接回答。

关于是否写文件：用户偏好"给我代码=只展示不落盘"，所以不写文件。

不过 workdir 是 D:\workspace\tiny-agent，这可能是 Go 项目。用户没说要放哪，先展示。

写代码时注意 go-redis v9 的 API：rdb.SetNX(ctx, key, value, ttl).Result() 返回 (bool, error)。正确。

另外可以提一下 SetArgs 的 Get/KeepTTL 之类，不必。

关于轮询间隔：可以稍微加个随机抖动退避避免同时重试（了解即可）。

给出代码，标注【逐行理解】等不用太重，简单标注关键行。

再补充一个"为什么不直接用 redis 客户端自带的重试"——go-redis 的 MaxRetries 是命令级重试，不能替代业务层的锁轮询，因为 SETNX 返回 false 是成功执行的命令、不会重试。

长度控制：中等。用户要基础代码。给完整可跑的最小示例（package main 里带 main 也行，或者只给三个函数 + 用法片段）。给函数 + 使用片段比较清爽。

写吧。

</details>

### 🤖 助手

##### 先回答你的问题

是红锁（Redlock）的实现，出自 `go-redis/redis_rate` 作者的同款库 **redsync**（`github.com/go-redsync/redsync`）。判断依据就是你说的那三处：

| 位置 | 单实例锁的特征 | 但这段代码 |
|---|---|---|
| ⑤ `actOnPoolsAsync` | 只有**一个** Redis 客户端 | **一组** pool **并发**去抢 |
| `n >= m.quorum` | 返回 bool 就完事 | 要**多数派**成功才算成功 |
| ⑥ `until = now + expiry - 耗时 - drift` | 直接 `SET NX PX` | 在租约有效期内还要扣除一轮网络往返耗时和时钟漂移 |

⑥ 那行在单实例里是**没有意义**的：单实例的租约就是你 `SET` 进去的 TTL，由 Redis 自己计时，不需要客户端再计算一遍。红锁必须这么做，是因为它在多个节点上分别设置了 TTL，各节点的时钟不同步，又加上这一轮请求消耗了时间，所以在你认为“我已经拿到锁”的时刻，实际剩余租期可能已经比 TTL 短了。

另外 ⑧ 那行（`if i == tries-1 && err != nil`）值得留意：它只在最后一轮才把网络错误向上返回，前面的轮次中即使有网络错误也会被吞掉、继续重试——这算是这段代码的一个隐藏行为。

##### 单实例 + 主动轮询的最小实现

以下三个函数，依赖 `github.com/redis/go-redis/v9`：

```go
package redislock

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ErrLockFailed = errors.New("lock failed: retries exhausted")
	ErrLockLost   = errors.New("lock lost: expired or not owner")
)

// ① 释放锁：只有 value 还是自己的才删，防止删掉别人的锁
//    【逐行理解】GET 比对 + DEL 必须是原子的，所以用 Lua（EVAL）
var releaseScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("del", KEYS[1])
end
return 0
`)

// ② 归属权凭据：随机值，保证“我加的锁只能我删”
func genValue() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// Lock 主动轮询加锁：最多试 tries 次，两次之间等 delay
// 返回 value（解锁时要用它证明“锁是我的”）
func Lock(ctx context.Context, rdb *redis.Client, key string, ttl time.Duration, tries int, delay time.Duration) (string, error) {
	value, err := genValue()
	if err != nil {
		return "", err
	}

	for i := 0; i < tries; i++ {
		if i != 0 { // 第一次不等待，直接抢
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay): // time.After 无需 Stop，泄漏的 timer 到期自动回收
			}
		}

		// ③ 单实例核心就这一行：SET key value NX PX ttl
		ok, err := rdb.SetNX(ctx, key, value, ttl).Result()
		if err != nil {
			// 网络抖动/连接问题：不是“被别人占了”，下一轮再试；
			// 最后一轮还错就如实上抛，别假装是抢不到
			if i == tries-1 {
				return "", err
			}
			continue
		}
		if ok {
			return value, nil // 抢到了
		}
		// ok == false：锁在别人手里，进入下一轮
	}
	return "", ErrLockFailed
}

// Unlock 释放锁：Lua 原子比对 + 删除
func Unlock(ctx context.Context, rdb *redis.Client, key, value string) error {
	n, err := releaseScript.Run(ctx, rdb, []string{key}, value).Int64()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLockLost // 已过期被回收，或 key 已被别人持有
	}
	return nil
}
```

使用方式：

```go
value, err := redislock.Lock(ctx, rdb, "lock:order:123", 5*time.Second, 10, 100*time.Millisecond)
if err != nil {
	return err // 抢不到 or ctx 取消 or redis 出错
}
defer func() {
	// 释放用独立的短 ctx，别用已被取消的业务 ctx，否则解锁这步直接失败
	uctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = redislock.Unlock(uctx, rdb, "lock:order:123", value)
}()

// ... 临界区
```

##### 与红锁逐点对照 + 取舍

**【值得记】**

1. **`SetNX` 在 go-redis 里就是 `SET NX PX`，一条命令完成“判断 + 设置 + 给过期时间”**。不要写成 `Exists` + `Set` 两步，那中间有 gap，也不要用 `SetNX` 不带 TTL（一旦进程崩溃就永久死锁）。
2. **解锁必须用 Lua**：`GET` 判断 value 再 `DEL` 这两步之间如果锁恰好过期、被别人拿走，你就会删掉别人的锁。这是网上手写分布式锁最常见的 bug，面试也最爱问。
3. **主动轮询和客户端重试不是一回事**。go-redis 的 `MaxRetries` 只重试“返回错误的命令”，而 `SETNX` 抢不到时是**正常返回 false**，客户端不会重试——所以业务层的轮询必须自己写。
4. **轮询间隔固定值会有羊群效应**：一批协程同一时刻起来，退避后再同一时刻撞上去。生产里建议 `delay` 加 ±30% 随机抖动（`delay/2 + rand(delay)`），或做指数退避。

**【知道就行】**

- 单实例锁的可用性天花板就是那台 Redis 的上限：主从异步复制下，主节点写完 `SET NX` 还没同步就宕机，从节点升主后你手里的锁就“消失”了。这才是红锁想解决的问题（代价是性能和部署复杂度，而且红锁本身在业界有争议，Martin Kleppmann 那篇《How to do distributed locking》值得看）。
- redsync 的 `until`/`driftFactor`/`quorum` 三件套在单实例代码里全部删掉，不是偷懒，而是它们的语义是为多节点服务的。
- 你贴的代码 ④ 里 `timer.Stop()` 是必须的——我这里用 `time.After` 就绕过了这个心智负担（Go 1.23 之后 `time.After` 的 timer 也能被 GC 及时回收了）。

需要我再加一个**自动续期 watchdog**（后台 goroutine 每 `ttl/3` 续一次，`Unlock` 时停掉）的版本吗？那是业务方“临界区可能超过 TTL”时才需要的，基础版故意没给。

### 👤 用户

lua原子比对体现在哪？是在releaseScript.Run这个函数吗？给给代码

## Turn 2

<details>
<summary>🧠 思考过程</summary>

用户问：lua 原子比对体现在哪？是在 releaseScript.Run 这个函数吗？给代码。

用户偏好：由浅入深、先大白话；给代码只展示不落盘；分块；值得记的点标出。

先回答核心：原子性不在 Run 这个 Go 函数里，Run 只是把脚本文本 + 参数发过去。原子性来自 Redis 服务端：EVAL 执行 Lua 脚本时，整个脚本作为一个命令在单线程里跑完，中间不插入其他客户端的命令。所以 GET 和 DEL 之间没有 window。

关键点：`redis.NewScript` 是客户端侧的封装（负责构造 EVAL/EVALSHA 命令、缓存 sha1），它自己不做任何原子保证。

给代码：
1. `redis.NewScript` 展开到底发出什么命令 —— 手写等价版本：`rdb.Eval(ctx, script, []string{key}, value)`，以及 `SCRIPT LOAD` + `EVALSHA` 的机制。
2. Lua 脚本逐行解释。
3. 对比错误写法（GET + DEL 两步）为什么有 gap，用时间轴图。
4. 顺便：为什么单实例下用 Lua 就够，红锁里 release 也是同样的脚本（redsync 的 release 也用 lua）。

还可以补充：Redis 7 之后有 `SET ... GET` 或者 `DEL if value`? 其实没有原生 CAS DEL。Redis 6.2+ 有 `GETDEL` 但那是无条件删。所以 Lua 是标准做法。另外 Redis 7.0 的单命令？

还有一种避免 Lua 的方式：把 key 设计成每次唯一（比如 lock:xxx:<value> 加个索引），但不实用。Lua 是标准。

另外提一个坑：`SCRIPT LOAD` 缓存 + `EVALSHA` 在 Redis 重启/主从切换（未持久化脚本缓存）时会 NOSCRIPT 错误，go-redis 的 Script.Run 会自动 fallback 到 EVAL。这是它封装的价值。

要不要真的查代码？用户问"是在 releaseScript.Run 这个函数吗"，我可以指出 go-redis 里 `Script.Run` 的实现路径。我没读过源码，不应编造行号。可以说实现要点而不假装引用源码行号。为了准确，我可以只说机制：`Script.Run` 内部调 `EvalSha`，遇到 NOSCRIPT 回退 `Eval`。这是 go-redis 已知行为，比较有把握。但为了避免编造细节，我可以表述为"内部走 EVALSHA、遇到 NOSCRIPT 自动回退 EVAL"——这是 go-redis 文档记载的行为（Script.Run 的注释确实这么说）。

用户要求给代码。给点：
- 用 Lua 的写法（已有）展开成等价的显式 Eval 写法。
- 服务端视角的命令序列。
- 错误写法的对比代码。

还要注意用户可能真正想问的是"源码里哪一行体现原子"。回答：没有任何一行 Go 代码体现原子性；Go 侧只是拼命令，原子性协议在 Redis 服务端（EVAL 的单线程执行模型）。Run 是**传输**，不是**保证**。

格式：简短结论 + 代码块 + 时间轴。标注【值得记】/【知道就行】。

关于长度：中等偏上，但不啰嗦。

给一个"错误写法"的代码：

```go
// ❌ 错误示范
cur, _ := rdb.Get(ctx, key).Result()
if cur == value {
    rdb.Del(ctx, key)   // ← 这里之前锁可能刚好过期、被别人重新抢走
}
```

时间轴：

```
客户端A                      Redis                    客户端B
  GET key -> "A"                                       
                           key 到期删除
                                                         SET NX key "B" 成功
  DEL key  ← 删掉了B的锁 💥
```

正确写法的执行序列：

```
EVAL "if redis.call('get',KEYS[1])==ARGV[1] then return redis.call('del',KEYS[1]) end return 0" 1 lock:order:123 "A"
```

Redis 内部：脚本作为一个整体执行，GET 与 DEL 之间没有其他命令插队（Redis 单线程执行命令；Lua 脚本执行期间不会被其他客户端命令打断）。

值得记的点里可以提：Redis 保证的是"脚本原子执行"，不保证"脚本执行失败回滚"——Lua 中途报错，前面写过的命令不回滚（没有事务回滚语义）。这是个高频面试追问。

还有 `redis.call` vs `redis.pcall`。

给代码时保持与前面回答同一包名 redislock，函数名 Unlock。

给三种实现对比：
1. NewScript（推荐，生产用）
2. 直接 Eval（脚本只执行一次时）
3. EVALSHA 手动（理解机制）

写吧。注意别太长。

关于"是不是在 releaseScript.Run"——我要明确指出：Run 只负责把它变成 EVALSHA 命令发出去；如果只看 Go 代码，你永远找不到原子性的实现，因为它在 Redis 服务端。这是很重要的一点。

另外可以给一个自测验证思路（用 redis-cli 直接 EVAL），但用户不喜欢"写验证代码先跑一遍"。这里给的是"如何观察"，不是排错。可以简单提一句 redis-cli 命令，作为理解手段，不落盘。

好，组织回答。

</details>

### 🤖 助手

##### 先给结论

不，原子性并不在 `releaseScript.Run` 里。`Run` 只做了一件事：把脚本 + 参数**打包成一条命令发出去**。原子性由 **Redis 服务端**保证——Lua 脚本在 Redis 里是**作为一个整体执行**的，GET 和 DEL 之间不可能插进别的客户端的命令。

所以你在 Go 代码里翻到天亮也找不到"原子"的实现代码，它属于协议层，不在客户端。

##### 一、`Run` 到底发出了什么

`redis.NewScript(...)` 只是个客户端侧壳子，它记下脚本文本、算一个 sha1。`Run` 内部走的是：

```go
// Script.Run 的等价展开（go-redis v9 的实际行为）
// ① 先试 EVALSHA（只发 40 字节 sha，省带宽）
// ② 服务端回 NOSCRIPT（重启/主从切换后脚本缓存丢了）→ 自动回退 EVAL 发全文
```

在 Redis 那边看到的命令就长这样（`redis-cli MONITOR` 能抓到）：

```
EVAL "if redis.call('get', KEYS[1]) == ARGV[1] then return redis.call('del', KEYS[1]) end return 0" 1 lock:order:123 "aB3x..."
```

`1` 是 key 的个数；后面依次是 `KEYS[1]`、`ARGV[1]`。脚本里那个 `if` 的**整个判断+删除**，就是这一条命令的内容。

##### 二、三种等价写法（从封装到底层）

```go
// ========== 用法 1：NewScript（推荐，生产用这个）==========
var releaseScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("del", KEYS[1])
end
return 0
`)

func Unlock(ctx context.Context, rdb *redis.Client, key, value string) error {
	n, err := releaseScript.Run(ctx, rdb, []string{key}, value).Int64()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLockLost // 锁已过期，或已不是我的
	}
	return nil
}
```

```go
// ========== 用法 2：直接 Eval（脚本只跑一次，不用缓存）==========
const releaseLua = `
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("del", KEYS[1])
end
return 0
`

func UnlockEval(ctx context.Context, rdb *redis.Client, key, value string) error {
	n, err := rdb.Eval(ctx, releaseLua, []string{key}, value).Int64()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLockLost
	}
	return nil
}
```

```go
// ========== 用法 3：手写 EVALSHA（只为理解机制，别上生产）==========
func UnlockManual(ctx context.Context, rdb *redis.Client, key, value string) error {
	sha, err := rdb.ScriptLoad(ctx, releaseLua).Result() // ① 先把脚本存进服务端
	if err != nil {
		return err
	}
	n, err := rdb.EvalSha(ctx, sha, []string{key}, value).Int64() // ② 之后只发 sha
	if err != nil {
		// NOSCRIPT: 服务端脚本缓存没了（重启/切主），退回发全文
		n, err = rdb.Eval(ctx, releaseLua, []string{key}, value).Int64()
		if err != nil {
			return err
		}
	}
	if n == 0 {
		return ErrLockLost
	}
	return nil
}
```

**【值得记】** 用法 3 里 `ScriptLoad` + `EvalSha` + `NOSCRIPT` 回退这三步，就是 `NewScript().Run()` 替你干的事——它顺带在本地还缓存了 sha1，不重复算。很多人以为 `NewScript` 有什么魔法，其实就这。

##### 三、"不原子"到底会坏在哪（这是重点）

```go
// ❌ 错误写法：两步
func UnlockWrong(ctx context.Context, rdb *redis.Client, key, value string) error {
	cur, err := rdb.Get(ctx, key).Result()  // 第一步：判断是不是我的
	if err != nil {
		return err
	}
	if cur != value {
		return ErrLockLost
	}
	rdb.Del(ctx, key)                        // 第二步：删 —— 中间有空隙！
	return nil
}
```

坏在时序上。假设 A 的锁 TTL 正好在这两步之间到期：

```
时间 →

A:  GET key → "A" ✔            ② 以为自己还持锁
                                  ↓
Redis:  key TTL 到期，自动删除
                                  ↓
B:                              SET key "B" NX → 成功，B 拿到锁
                                  ↓
A:                              ③ DEL key  ← 把 B 的锁删了 💥
```

A 删掉 B 的锁之后，B 还在临界区里跑，同时 C 又能立刻抢到锁——**互斥直接被击穿**。这个 bug 在生产里极难复现（窗口是微秒级），但一旦在高峰期出现就是数据错乱。

而 Lua 版没有这个窗口：

```
A:  EVAL(if get==ARGV then del)  ← 这一整条命令在 Redis 主线程里一口气跑完
                                    get 和 del 之间，B 的 SETNX 排不进来
```

##### 四、配套的两个细节

**【值得记】Redis 的"原子"≠"事务回滚"。** Lua 脚本保证的是**不被其他客户端命令打断**（isolation），不保证出错回滚——脚本里如果前面已经 `del` 了、后面 `redis.call` 报错，前面的写操作不会撤销。要写多步修改的脚本，必须自己保证逻辑顺序。这是面试高频追问。

**【知道就行】** 脚本里 `redis.call` 出错会抛异常中断整个脚本并返回错误给客户端，`redis.pcall` 则把错误当返回值继续跑。上面这段用的是 `redis.call`，语义上更严格，没问题。

**【知道就行】** 你可以用 `redis-cli` 直接观察，不用写 Go：

```
redis-cli EVAL "return redis.call('get', KEYS[1])" 1 lock:order:123
```

看到返回值就说明这条路通了，`Run` 包的东西一层不剩。

要不要我接着把**续期 watchdog** 那版写出来？那版里面 `Unlock` 还得处理"续期 goroutine 和手动 Unlock 撞车"的问题，比这复杂点。

### 👤 用户

也就是说lua就是个脚本是吧？如果是你给我看看这个脚本长什么样。代码是什么

## Turn 3

<details>
<summary>🧠 思考过程</summary>

用户问：Lua 就是个脚本是吧？给我看看这个脚本长什么样，代码是什么。

用户想理解：Lua 是什么、这个脚本长什么样。由浅入深，先大白话类比再上术语。用户偏好【逐行理解/理解/略读】标注。

要点：
1. Lua 是一种语言（嵌入式脚本语言，很小，Redis 内置了解释器）。
2. 我们那段脚本的内容就是那几行，展开逐行讲。
3. Redis 里 Lua 的特殊约定：KEYS[1]、ARGV[1] 是什么；redis.call 是什么。
4. 原子性的真正来源：Redis 单线程 + 脚本执行期间不处理其他命令。
5. 给脚本的不同版本：release 脚本，以及可能的 acquire 脚本（其实 acquire 用 SET NX 就够）。还可以给一个"续期"脚本作为例子，展示 Lua 的更多语法（if/else/return/tostring）。用户可能想看"Lua 代码是什么样子"，所以给一个稍多语法的例子有帮助。
6. 注意：不要在 Redis 里跑阻塞操作。

要强调：这个脚本不是"我写的伪代码"，是真正发到 Redis 服务端执行的 Lua 源码，只有 5 行。

Lua 语法要点讲清楚：没有分号、没有类型声明、`redis.call("命令", 参数...)`、`==`、`then/end`、`return`、`#KEYS` 之类。

还可以说明 KEYS/ARGV 是 Redis 注入的两个全局数组（table），索引从 1 开始——这是个大坑，其他语言从 0 开始。值得标【值得记】。

另外：为什么要用 KEYS 传 key 而不是直接拼字符串？因为 Redis Cluster 要校验 key 数量与分发；官方规定所有 key 必须走 KEYS 参数。这是值得记的点。

再给一个直观的"脚本怎么被调用的完整流程"。

还可以提供一个获取锁的 Lua 版本（虽然不必要），用来对比"什么时候不需要 Lua"。或者给一个真实用到的：限流脚本、续期脚本。

回答结构：
1. 一句话：是脚本，而且短得离谱，就 5 行。
2. 看它在文件里长什么样（代码块，带行号注释）。
3. 逐行拆解【逐行理解】。
4. Lua 语法速览对照表（和 Go 对比）—— 这是用户最想看的"这语言长啥样"。
5. Redis 的两个硬约定：KEYS/ARGV 从 1 开始；key 必须放 KEYS。
6. 稍复杂一点的脚本示例（续期 + 限流），展示语法。
7. 小结 + 后续。

用户偏好简洁，但这是概念讲解，可以稍长。注意标注分档。

不落盘文件。

关于 Lua 的语法细节要准确：
- `redis.call('get', key)` 返回 bulk string 或 false（key 不存在时返回 false）
- 比较：`redis.call("get", KEYS[1]) == ARGV[1]` —— 注意 GET 不存在时返回 false，false == "xxx" 是 false，所以安全。这是个值得记的点。
- `redis.call("del", KEYS[1])` 返回整数
- return 0
- 数字在 Lua 里是双精度浮点，返回整数用 return 0 会转成 integer 回复？实际上 Redis 会把 Lua number 转为 integer。这里有细节：Lua 的 number 是 double，Redis 返回时截断为整数。要知道就行。

go-redis 把返回值 .Int64()。

还有：脚本里访问未定义变量返回 nil，拼写错误不会报错（Lua 坑）。可以不提。

再看续期脚本示例：

```lua
-- 续期：只有 key 还是我的才延长 TTL
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("pexpire", KEYS[1], ARGV[2])
end
return 0
```

这个很有用，也展示 ARGV[2]。

再给个稍复杂的，展示 if/elseif/for：

```lua
-- 令牌桶限流（只是展示语法，不是重点）
local tokens = tonumber(redis.call("get", KEYS[1]) or "0")
```

嗯，别塞太多，聚焦。给续期一个就够，再给一个"展示语法全貌"的小例子（for 循环、local、tonumber）。

Lua 语法速览表很有价值：

| Lua | Go | 说明 |
|---|---|---|
| 无类型声明 `local x = 1` | `x := 1` | |
| `if a == b then ... end` | `if a == b { ... }` | 必须有 then/end |
| `and or not` | `&& \|\| !` | |
| `~= ` | `!=` | |
| 字符串 `'x'` 或 `"x"` | 双引号 | |
| 数组从 1 开始 | 0 开始 | 大坑 |
| `nil` | `nil` | 未定义/不存在 |
| `#t` | `len(t)` | 长度 |
| 注释 `--` | `//` | |
| `..` 拼接字符串 | `+` | |

好，写。

注意用户语言：简体中文。

最后问一句是否需要继续。上一轮已经问过 watchdog，这轮不要再重复问同样的问题，可以简短收尾。

长度：中等。别太长。

</details>

### 🤖 助手

##### 是脚本，而且短得不像话

Lua 是一门独立的编程语言（很小，解释器才一百多 KB），Redis **内置了 Lua 解释器**，所以你发一段 Lua 源码过去，Redis 就在自己的进程里把它跑完。那段"原子操作"的全部代码，就是这几个字符：

```lua
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("del", KEYS[1])
end
return 0
```

没有 import，没有 package，没有 class，5 行。就这么多。

##### 逐行拆解

```lua
if redis.call("get", KEYS[1]) == ARGV[1] then
```
**【逐行理解】**
- `redis.call(...)` 是 Redis 往 Lua 里注入的一个函数，作用等于"在脚本里执行一条 Redis 命令"。`redis.call("get", KEYS[1])` 就是在执行 `GET lock:order:123`。
- `KEYS[1]` / `ARGV[1]` 是 Redis 在执行前注入的**两个全局数组**，装着你调用时传的参数。
- `==` 是相等比较（不是 C 的 `=` 赋值）。
- `then` 是 Lua 的固定语法，相当于 Go 的 `{`。

```lua
    return redis.call("del", KEYS[1])
```
**【逐行理解】** 删掉这个 key，并把这个删除的返回值（整数 1）当作整个脚本的返回值。

```lua
end
return 0
```
**【逐行理解】** `end` 结束 if。如果没进 if（说明锁不是我的），返回 0，Go 侧 `n == 0` 就知道"锁丢了"。

所以 Go 里 `releaseScript.Run(ctx, rdb, []string{key}, value)` 传的两个东西，在 Lua 那边就是：

| Go 侧 | Lua 侧 | 值 |
|---|---|---|
| `[]string{key}` | `KEYS[1]` | `"lock:order:123"` |
| `value` | `ARGV[1]` | `"aB3x..."` |

##### 【值得记】两张必须知道的表

**一、Lua 语法速览（和 Go 对照）**

| Lua | Go | 备注 |
|---|---|---|
| `local x = 1` | `x := 1` | 加 `local` 才是局部变量，不加就是全局（Redis 里会跨脚本污染，很容易出问题） |
| `if a == b then ... end` | `if a == b { ... }` | `then`/`end` 不能省 |
| `and` / `or` / `not` | `&&` / `\|\|` / `!` | |
| `~=` | `!=` | 不等的写法很反直觉 |
| `--` | `//` | 单行注释 |
| `#t` | `len(t)` | 取长度 |
| `a .. b` | `a + b` | 字符串拼接 |
| `nil` | `nil` | 未定义/不存在 |
| **数组从 1 开始** | 从 0 开始 | `KEYS[1]` 不是 `KEYS[0]`，这是所有人第一次写都会错的地方 |

**二、Redis 对脚本的两个硬约定**

1. **所有 key 必须通过 `KEYS[]` 传，不能写死在脚本字符串里。** 因为 Redis Cluster 要靠这个数组来判断"这些 key 在不在同一个槽"，决定把脚本发给哪个节点。写死的话集群下会直接报错。
2. **返回值有限制。** Lua 的 number 是浮点数，返回给客户端时会被截断成整数；返回 `false` 会变成 nil；返回 table 会变成数组（遇到 nil 就截断）。

##### 再看一个稍复杂点的，感受下 Lua 长什么样

这是"续期"脚本——用来给 watchdog 延长 TTL 的：

```lua
-- 只有 key 还是我的才续期，顺便返回新的剩余毫秒数
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("pexpire", KEYS[1], ARGV[2])
end
return 0
```

Go 侧调用就多传一个参数：

```go
var renewScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
    return redis.call("pexpire", KEYS[1], ARGV[2])
end
return 0
`)

func Renew(ctx context.Context, rdb *redis.Client, key, value string, ttl time.Duration) (bool, error) {
	n, err := renewScript.Run(ctx, rdb, []string{key}, value,
		strconv.FormatInt(ttl.Milliseconds(), 10)).Int64()  // ← ARGV[2] 必须是字符串
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
```

**【值得记】** `ARGV` 里所有东西到了 Lua 那边**都是字符串**（或者 nil），没有 int。要算数必须自己 `tonumber()`。所以 Go 侧传 `ttl.Milliseconds()` 得先 `strconv.FormatInt` 转字符串——直接传 `int64` 到 `Run` 的 `args ...interface{}` 里，go-redis 会帮你转，但显式转更不容易踩坑。

##### 什么时候才需要写 Lua

**【值得记】** 判断标准只有一条：**"我要做的这件事，能不能用一条 Redis 原生命令完成？"**

- 加锁 → 能（`SET key val NX PX ttl`），所以**不需要 Lua**。
- 解锁 → 不能（没有 "value 匹配才删" 的原生命令），所以要 Lua。
- 续期 → 不能（没有 "value 匹配才 PEXPIRE" 的原生命令），所以要 Lua。

Lua 不是为了炫技，是**给 Redis 补一个"条件执行"的能力**。不要什么都往脚本里塞——脚本会阻塞 Redis 主线程，里面千万别写循环十万次的代码，也别在里面 `GET` 大 key。

要不要我把带续期 watchdog 的完整版（`Lock` / `Unlock` / 后台续期 goroutine，以及它和手动 Unlock 撞车的处理）写出来？
