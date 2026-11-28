# NexusFlow 文档中心

按你的角色选择入口：

| 角色 | 推荐阅读 | 内容 |
| --- | --- | --- |
| 首次体验 | [快速上手](./quickstart.md) | 10 分钟启动、导入示例数据、走通上传→检索→问答→工作流→审批 |
| 业务/运营 | [用户使用指南](./user-guide.md) | 控制台各页面、知识库与权限、检索问答、审批、API Key、Webhook、连接器、常见问题 |
| 集成开发 | [用户使用指南 · API 速查](./user-guide.md#4-api-速查) | 认证方式、接口清单、OpenAI 兼容端点、Webhook 签名 |
| 运维/SRE | [部署指南](./deployment.md) | Kubernetes 部署、HPA、存储、沙箱后端、功能开关与回滚 |
| 架构评审 | [系统架构](./architecture.md) | 服务边界、数据流、检索与 Agent 调度设计 |
| 数据/平台 | [数据模型](./schema_enterprise_v1_1_to_1_5.sql) | 企业版表结构（身份、知识库、审批、审计等） |
| 质量验证 | [全流程验证证据](./e2e-evidence.md) | 一次真实运行的 ID、召回数量、引用与工作流结果 |
| 项目展示 | [项目海报（HTML）](./poster.html) / [PNG](./poster.png) | 一页式项目介绍，可直接投屏或导出图片 |

其他资源：

- [README](../README.md)：仓库总览、服务目录、开发者命令、实现边界
- [ROADMAP](../ROADMAP_V1_1_V1_5.md)：版本规划
- [示例数据](../examples/demo)：示例文档与可运行工作流
- [种子脚本](../scripts/seed_demo_data.py)：一键导入示例知识库、账号与 API Key
- [全流程脚本](../scripts/e2e_demo.py)：生成最新验证证据

## 文档维护约定

- 用户可见的行为、权限、默认开关发生变化时，同步更新 `user-guide.md` 与 `quickstart.md`。
- 新增示例或调整示例目录结构时，更新 `examples/demo/README.md`。
- 发布前运行 `python scripts/e2e_demo.py --output docs/e2e-evidence.md` 刷新证据。
