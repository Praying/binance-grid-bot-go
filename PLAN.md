# Binance Grid Trading Bot - 改进计划

## 1. 项目目标

本项目旨在对现有的 `binance-grid-bot-go` 进行功能增强和架构优化，以提高其稳定性、可测试性和可维护性。

主要实现以下几个核心目标：

1.  **支持币安测试网 (Testnet Support):** 能够在不使用真实资金的情况下，安全地测试和验证策略。
2.  **增加回测功能 (Backtesting):** 利用历史数据对交易策略进行模拟、评估和优化。
3.  **代码结构重构 (Code Refactoring):** 优化项目结构，使其更清晰、更易于扩展。
4.  **实现状态持久化 (State Persistence):** 确保机器人在重启后能够恢复之前的运行状态，提高稳定性。

## 2. 架构设计

为了实现功能解耦，特别是为了支持回测，我们将引入一层 `Exchange` 接口，将核心交易逻辑与具体的交易所实现分离。

```mermaid
graph TD
    subgraph "当前架构 (Current Architecture)"
        A[GridTradingBot] -- 直接调用方法 --> B(BinanceClient)
        B -- 发送HTTP请求 --> C[币安实时API (Binance Live API)]
        D[币安WebSocket] -- 推送价格 --> A
    end

    subgraph "建议架构 (Proposed Architecture)"
        E[main] -- 根据模式选择 --> F{启动模式: Live 或 Backtest}
        F -- Live --> G[实时交易模式]
        F -- Backtest --> H[回测模式]

        subgraph G
            I[GridTradingBot] -- 调用接口 --> J(Exchange 接口)
            J -- 由LiveExchange实现 --> K[LiveExchange(包装BinanceClient)]
            K -- 发送HTTP请求 --> L[币安API (生产/测试网)]
            M[币安WebSocket] -- 推送价格 --> I
        end

        subgraph H
            N[GridTradingBot] -- 调用接口 --> O(Exchange 接口)
            O -- 由BacktestExchange实现 --> P[BacktestExchange (模拟器)]
            Q[历史数据 (如CSV文件)] -- 模拟价格推送 --> N
            N -- 下单/查询 --> P
            P -- 模拟成交/更新状态 --> N
        end
    end
```

## 3. 实施步骤

我们将分阶段进行开发，以确保每个步骤都能得到充分的测试和验证。

### 阶段一: 项目结构重构

这是后续所有工作的基础。

1.  **创建目录结构:**
    -   `cmd/bot/`: 存放程序入口 `main.go`。
    -   `internal/config/`: 负责配置文件的加载和管理。
    -   `internal/models/`: 定义所有数据结构 (如 `Order`, `Position`, `Config` 等)。
    -   `internal/exchange/`: 存放 `Exchange` 接口、`LiveExchange` 和 `BacktestExchange` 的实现。
    -   `internal/bot/`: 包含 `GridTradingBot` 的核心交易逻辑。
    -   `internal/logger/`: 日志模块。
    -   `data/`: (可选) 存放回测用的历史数据 (如 `BTCUSDT-1h.csv`)。

2.  **迁移代码:** 将 `main.go` 中的代码按照上述结构拆分并迁移到对应的文件中。

### 阶段二: 核心功能开发 (测试网 & 接口解耦)

1.  **实现测试网支持:**
    -   在 `internal/models/config.go` 的 `Config` 结构体中增加 `IsTestnet bool` 字段。
    -   修改配置加载逻辑，根据 `IsTestnet` 的值动态设置正确的币安 API 和 WebSocket URL。

2.  **定义并实现 `Exchange` 接口:**
    -   在 `internal/exchange/exchange.go` 中定义 `Exchange` 接口。
    -   创建 `live_exchange.go`，实现 `LiveExchange` 结构体，该结构体包装现有的 `BinanceClient` 并实现 `Exchange` 接口。
    -   重构 `GridTradingBot`，使其依赖于 `Exchange` 接口，而不是具体的 `BinanceClient`。

### 阶段三: 回测框架开发

1.  **创建模拟交易所 (`BacktestExchange`):**
    -   在 `internal/exchange/backtest_exchange.go` 中创建 `BacktestExchange`。
    -   实现 `Exchange` 接口的所有方法，但其内部逻辑是基于内存状态的模拟 (模拟账户、持仓、订单成交)。

2.  **数据加载与回测循环:**
    -   在 `cmd/bot/main.go` 中增加启动模式选择 (例如通过命令行参数 `--mode=live` 或 `--mode=backtest`)。
    -   在回测模式下，实现从 CSV 文件加载历史数据的逻辑。
    -   创建一个回测主循环，遍历历史数据，调用 `GridTradingBot` 的策略执行方法。

3.  **开发性能报告模块:**
    -   回测结束后，计算并输出关键性能指标 (KPIs)，例如：总收益率、最大回撤、夏普比率、胜率等。

### 阶段四: 稳定性和健壮性增强

1.  **实现状态持久化:**
    -   在 `GridTradingBot` 中增加 `SaveState()` 和 `LoadState()` 方法。
    -   在关键操作后 (如下单、取消订单) 或定期将机器人的当前状态 (如 `gridLevels`) 保存到 JSON 文件中。
    -   在机器人启动时，尝试从该文件加载状态，以实现断点续传。

2.  **引入结构化日志:**
    -   在 `internal/logger/` 中集成 `logrus` 或 `zap` 库。
    -   在整个项目中用新的日志模块替换标准的 `log`。

## 4. 下一步

该计划文档创建完毕后，我们将请求切换到 **Code** 模式，并严格按照此计划开始编码实现。