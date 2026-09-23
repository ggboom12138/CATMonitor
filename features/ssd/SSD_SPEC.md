# SSD 监控（ssd）设计规格

> **文档定位**：本文件描述 CATMonitor 的 SSD 监控特性（`features/ssd`）的设计与规格。
> 对应代码：`features/ssd/`（只读消费端）+ `internal/collectors/disk/` 的 SMART 明细与
> 整盘使用率子方法 + `internal/source/smartctl|sys` 的扩展。
> 核心原则：**daemon 是唯一采集者与 snapshot 生产者；ssd 二进制只读消费；所有指标一律
> 按盘（device 标签）；默认不启用（opt-in）；零新依赖（Go 标准库 + 既有 smartctl 外部命令）。**

## 1. 概述

### 1.1 目标

为服务器上的每块 SSD 提供独立可视化页面（单独二进制 `catmonitor-ssd`、单独端口 `:19324`）：

- **盘清单与识别**：SSD 数量、型号、序列号、固件、接口、容量；SSD/HDD 介质识别
  （`/sys/block/*/queue/rotational` + smartctl JSON `rotation_rate`）。
- **整盘容量/使用率**：`device_space_usage` / `device_space_detail` —— 采集端把分区与
  LVM 逻辑卷（`/dev/mapper/*` → dm-N → slaves）归属到物理盘后聚合，**不是挂载点粒度**。
- **读写状态**：吞吐、IOPS、读写延迟（速率，daemon 侧由 diskstats 差值算出）+ 累计读写字节。
- **SMART 明细**：健康状态、温度、磨损百分比、TBW（累计写入量）、通电时长/次数、
  介质错误、重映射扇区、可用备件、意外断电。
- **RAID 卡后面的物理盘**：经 `smartctl --scan` 发现 megaraid 等穿透通道，
  `smartctl -j -a -d megaraid,N /dev/bus/0` 读取，device 标签即通道名（如 `megaraid,0`）。

### 1.2 原则

- **全按盘**：本特性采集的每个指标都带 `device` 标签；非按盘指标（挂载点粒度的
  `space_usage`、系统级 `io_wait`/`io_errors`）不进 `features/ssd/metrics.yaml`，不采集。
- **只读消费**：`catmonitor-ssd` 不做任何采集，只读 daemon 产出的
  `snapshot.json` + `snapshot_disk.json`（原子写，读者看不到半文件）。
- **默认关闭**：需在 `catmonitor.yaml` 的 `features:` 列表加入 `ssd` 才启用；未启用时
  新指标零采集、smartctl 零调用。
- **优雅降级**：RAID 逻辑卷无 SMART → 显示 N/A；SCSI/SAS 传输层无磨损字段 → 不产出该序列；
  smartctl 不存在/无权限 → 负缓存后静默跳过。

### 1.3 架构

```
                 ┌─────────────────── daemon（已有） ───────────────────┐
 /sys/block/*    │ disk collector                                        │
 /proc/mounts ──►│  ├─ collectDeviceUsage   （LVM 归属解析 → 整盘聚合）  │
 /proc/diskstats │  └─ collectSMARTDetailed （直连盘 + RAID 穿透通道）   │──► snapshot_disk.json
 smartctl -j -a  │        ▲ smartctl source 层（60s 缓存 + 负缓存）      │      + snapshot.json
 smartctl --scan │ ───────┘                                               │        │
                 └────────────────────────────────────────────────────────┘        ▼
                                                                        ┌─ catmonitor-ssd ─┐
                                                                        │ /api/ssd  按盘视图│
                                                                        │ /        SPA 页面 │
                                                                        └──────────────────┘
```

## 2. 目录结构

```
features/ssd/
├── main.go            # 入口：-addr :19324、-snapshot-dir；端口占用自动 +1
├── handler.go         # Register + /api/ssd + SPA 路由；请求时读 snapshot
├── filter.go          # buildDiskViews：specs+metrics → 三层视图（纯函数）
├── embed.go           # //go:embed static
├── metrics.yaml       # ★ 特性指标作用域（白名单 + interval 2s），全按盘
├── static/            # 前端（原生 JS + Canvas，零依赖）
│   ├── index.html     # 页面壳（主题预置脚本）
│   ├── ssd.js         # 轮询 + 三层渲染 + Canvas 折线（60 点滚动缓冲）
│   └── ssd.css        # 深/浅双主题
├── filter_test.go     # 数据组装（含 RAID/逻辑卷/故障盘场景）
├── handler_test.go    # HTTP 层（API/SPA/静态资源/snapshot 缺失 503）
└── sole_scope_test.go # 唯一启用本特性时的自足性验证
```

## 3. 核心设计

### 3.1 数据组装（filter.go，纯函数）

`buildDiskViews(specs, metrics) → ([]DiskView, Overview)`：

1. **盘清单**：`snapshot_disk.json` 的 `specs` 中 `disk_info` 且 `media=ssd` 的行构成
   DiskView 基座（型号/序列号/固件/接口/容量）。`media` 由 hwinfo 在 daemon 侧标注：
   直连盘读 rotational，RAID 物理盘用 smartctl JSON 的 `rotation_rate`/`protocol`。
2. **指标归属**：metrics 按 `labels.device` 匹配到 DiskView，分三个维度：
   - `smart_*` → SmartView（传输层不提供的字段保持 nil，JSON 省略）；
   - `device_space_usage`/`device_space_detail(field)` → SpaceView；
   - `throughput(direction)`/`iops(direction)`/`*_latency`/`*_sectors_total` → IOView。
3. **非 SSD 设备的指标**（无匹配 spec 行）被忽略；系统级无 device 标签的指标同样忽略。
4. **Overview**：数量、总容量、平均使用率（仅有数据的盘参与）、健康计数
   （passed=1/0/无 SMART 三态）、最高磨损。

### 3.2 API

`GET /api/ssd` → `SSDResponse`：

```json
{
  "session_id": "1690000000", "version": "v0.3.6",
  "timestamp": "2026-09-23 10:00:00", "refresh_interval_ms": 2000,
  "overview": {"ssd_count":2, "total_capacity_gb":2879.5, "avg_space_usage":61.18,
                "healthy_count":1, "failed_count":0, "no_smart_count":1, "max_wear_percent":1},
  "disks": [
    {"device":"megaraid,0", "model":"SAMSUNG MZ7LH960HAJR-00005", "serial":"S45NNA0N662029",
     "interface":"SATA", "capacity_gb":960.2,
     "smart":{"passed":1, "temperature":37, "wear_percent":1, "power_on_hours":28599,
              "power_cycles":122, "data_written_gb":5500.88, "reallocated_sectors":0}},
    {"device":"sdb", "model":"MR9440-8i", "capacity_gb":1919.3,
     "space":{"usage_percent":61.18, "total_gb":1750.5, "used_gb":1070.8, "avail_gb":679.7},
     "io":{"read_throughput_mb_s":12.5, "write_throughput_mb_s":3.2, "read_iops":320, ...}}
  ]
}
```

snapshot 未就绪（daemon 未启动/未开 snapshot）→ `503 {"error":"snapshot not ready"}`。

### 3.3 前端（三层页面）

- **概览层**：SSD 数量 / 总容量 / 平均使用率 / 健康状态（正常·异常·无 SMART）/ 最高磨损。
- **卡片层**：每盘一卡——健康灯（绿/红/灰）、温度、累计写入、磨损条、使用率条；
  RAID 阵列成员的使用率显示 N/A。
- **详情层**：点选卡片 → SMART 属性表（阈值着色：磨损≥80 红、≥60 黄；介质错误>0 红）
  + 三张实时曲线（吞吐/IOPS/延迟，读蓝写橙双线，60 点滚动，前端 3s 轮询可调）。
- session_id 变化（daemon 重启）自动清空滚动缓冲；深/浅主题跟随系统并可切换。

### 3.4 采集端（daemon 侧，见对应文件）

- `internal/source/smartctl/health.go`：`Scan()`（`--scan -j`）与
  `HealthJSON(devPath, devType)`（`-j -a [-d devType]`），NVMe health log 与 ATA 属性表
  归一化；smartctl 退出码携带健康位（如 FAILED）时只要 JSON 完整即按成功解析，
  避免故障盘被负缓存隐藏。
- `internal/collectors/disk/smart_detailed.go`：`collectSMARTDetailed` 对直连盘 +
  scan 发现的穿透通道产出 11 项 SMART 指标；启用后旧 `-H` 文本路径自动让位防重复。
- `internal/collectors/disk/device_usage.go`：`collectDeviceUsage` 把挂载点归属到整盘
  （`/dev/sdb2`→sdb；`/dev/mapper/X`→dm-N→slaves→物理盘；nvme 分区 `pN` 后缀剥离），
  跨盘条带 LV 无法归属唯一盘时跳过。

## 4. 配置

```yaml
# catmonitor.yaml —— 默认不启用，手动加入 features 列表开启
features: [web, dfee, health, ssd]     # 加 "ssd"
snapshot:
  enabled: true                        # 前提（web/dfee 同样依赖）
  dir: /var/lib/catmonitor/snapshot
```

运行：`catmonitor-ssd -addr :19324 -snapshot-dir /var/lib/catmonitor/snapshot`

RAID 物理盘的 SMART 读取要求 smartctl 可用且有相应权限（root）；容器部署时镜像内需
含 smartmontools（`docker/catmonitor.yaml` 注释已说明）。

## 5. daemon 集成

本特性**不需要改 daemon 代码**：`features/ssd/metrics.yaml` 声明白名单后，既有
`loadConfig → SetFeatureScope/LoadFeatureOverrides/ComponentIntervals` 机制自动生效
（disk 采集节奏 2s = 本特性声明值与其他 feature 取 min）。构建经 `make ssd`
（`all` 依赖已含）。

## 6. 测试

| 文件 | 覆盖 |
|---|---|
| `internal/source/smartctl/health_test.go` | scan/JSON 解析（真机 ATA/SCSI fixture + NVMe 标准结构）、缓存/负缓存、传输层缺省字段 |
| `internal/source/sys/sys_test.go` | Rotational、BlockNames、DMName/DMSlaves |
| `internal/collectors/disk/ssd_metrics_test.go` | 明细指标产出（直连+RAID+逻辑卷降级）、整盘使用率聚合（LVM 场景）、归属解析 |
| `features/ssd/filter_test.go` | 三层视图组装（RAID 成员/逻辑卷/故障盘/空输入） |
| `features/ssd/handler_test.go` | API 200/503、SPA/静态资源、指标 JSON 形态往返 |
| `features/ssd/sole_scope_test.go` | 唯一启用 ssd 时全部所需指标在白名单内；非按盘指标被排除 |

## 7. 关键设计决策

| 决策 | 选择 | 理由 |
|---|---|---|
| 数据通路 | 只读消费 snapshot | 仓库哲学：daemon 唯一采集者，避免重复跑硬件 |
| SMART 解析 | `smartctl -j`（JSON） | NVMe/SATA/SCSI 三种输出结构统一，远稳于文本解析 |
| RAID 通道识别 | scan 条目 Type 含逗号 | 通用于 megaraid/cciss/3ware 等穿透通道，无需硬编码 |
| RAID 物理盘标签 | `megaraid,0`（即 -d 选择器） | 与系统盘名空间无冲突，页面直观显示通道 |
| 使用率粒度 | 采集端聚合（新指标） | 用户要求全按盘；handler 聚合需先采挂载点指标，违背该要求 |
| 磨损归一化 | NVMe percentage_used；ATA `100 - Wear_Leveling_Count 归一值` | 两条传输链的主流约定；未知厂商显示 N/A |
| 温度来源 | JSON `temperature.current` | smartctl 已归一化（ATA 原始值可能是打包的乱码） |
| 旧 `-H` 路径 | 启用明细时让位 | 防止 smart_status/smart_temperature 双份产出；旧路径保留供未启用 ssd 的场景 |
| 历史曲线 | 前端 60 点滚动缓冲 | snapshot history 只有跨盘 max 序列，无 per-device 历史 |
| 端口 | :19324 | 19320 exporter / 19321 faultsub / 19322 web / 19323 dfee 顺延 |

---
文档版本：v1.0 · 对应代码：features/ssd@feature/ssd-monitor · 2026-09-23
