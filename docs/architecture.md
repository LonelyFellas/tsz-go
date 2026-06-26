# 天生会背 — 技术架构方案

> 基于产品需求脑图整理，与当前 `tsz-go` 仓库（Go 模块化单体）对齐。

---

## 1. 产品概览

**天生会背** 是英语词汇记忆产品。层级上**只有班级**：用户可归属班级，教师按班带课，管理员在后台维护班级与内容。

| 端 | 说明 |
|----|------|
| **学员端** | 登录注册、当前学习集、多题型练习、学习统计 |
| **教师端** | 本班管理、学习进度报告、词表配置（不可见学员联系方式） |
| **管理后台** | 班级、教师、词表、题库、标准词库维护 |
| **落地页** | 首页、功能入口（低优先级：资源下载） |

**关键非功能需求：**

- **权限**：教师只看自己的班；管理员看全部
- **学习模型**：词项覆盖、题型轮换、优先级
- **多媒体**：TTS 发音、连读显示、音频校正
- **外部集成**：第三方词典、COCA 词频、科大讯飞 TTS
- **隐私**：教师侧联系方式等字段脱敏

---

## 2. 架构选型

### 推荐：模块化单体 + 前后端分离

```
┌──────────────────────────────────────────────────────────────┐
│           学员 App │ 教师 Web │ 管理后台 │ 落地页              │
└────────────────────────────┬─────────────────────────────────┘
                             │ HTTPS / REST
┌────────────────────────────▼─────────────────────────────────┐
│                      tsz-go API                               │
│   class │ vocabulary │ learning │ progress │ question │ …    │
└────────────┬───────────────────┬─────────────────────────────┘
             │                   │
        PostgreSQL          OSS（音频）
```

| 方案 | 结论 |
|------|------|
| **模块化单体** | **首选**：业务耦合在「学习 ↔ 词表 ↔ 班级」，单库事务简单 |
| 微服务 | 规模与团队未到拆分边界前不引入 |
| Serverless | 学习会话有状态、TTS 批量任务，不适合 |

**后期可按需加：**

- Redis + [asynq](https://github.com/hibiken/asynq)：TTS、报告导出等异步任务
- Redis 缓存：标准词库热读
- 读副本：统计查询

---

## 3. 系统边界

```
              ┌──────────────────────────┐
              │        管理后台           │
              │  班级 / 词库 / 题库 / 教师  │
              └────────────┬─────────────┘
                           │
                ┌──────────┴──────────┐
                │                     │
         ┌──────▼──────┐      ┌──────▼──────┐
         │   教师端     │      │   学员端     │
         │ 本班/词表/报告│      │ 学习/统计   │
         └─────────────┘      └─────────────┘
```

- 统一 API：`/api/v1/...`
- 鉴权：**角色 + 班级范围**（如 `class_id IN (...)`）
- 前端独立仓库，OpenAPI 生成客户端类型

**用户与班级：**

- 绑定班级的用户：使用教师/管理员下发的词表
- 未绑定班级的用户：自行维护词表与学习集（可购会员，见需求脑图）

---

## 4. 后端模块

垂直切片：`handler → service → repository`。

```
internal/
├── config/
├── auth/           # JWT、角色、班级范围
├── class/          # 班级、用户-班级关系、教师任课
├── user/           # 账号、登录注册（已有）
├── vocabulary/     # 词表、标准/自定义词库
├── question/       # 题库
├── learning/       # 学习集、会话、学习模型引擎
├── progress/       # 学习统计、教师报告
├── integration/    # 词典、COCA、科大讯飞 TTS
├── media/          # 音频、OSS
└── platform/
    ├── database/
    ├── httpserver/
    ├── queue/      # 后期
    └── storage/    # 后期
```

| 模块 | 职责 |
|------|------|
| `class` | 班级 CRUD、用户归属班级、教师与班级关系 |
| `vocabulary` | 词表文件夹、排序（教材序/字母/COCA）、标准库引用 |
| `question` | 选择/填空/完形/阅读等题型 |
| `learning` | 学习模型调度、当前学习集、答题记录 |
| `progress` | 学员统计；教师报告仅限本人布置的词表 |
| `integration` | 外部 API 防腐层 |
| `media` | TTS 任务、音频存储 |

---

## 5. 学习模型引擎

独立为 `learning` 域内的纯函数 + 状态机，便于单测。

**规则：**

1. 一轮内尽量让每个词至少出现一次
2. 相邻题目题型不重复
3. 高优先级词优先

```
LearningSet
  ├── WordItems[]      词 + 优先级 + 掌握度
  ├── QuestionTypes[]  闪卡、判断、选择、有声/无声拼写…
  └── Scheduler          输出下一题

Scheduler：
  - 每词「本轮是否已出现」
  - 记录上一题题型
  - 候选排序：priority DESC → 未出现 → 掌握度 ASC
  - 选词 + 选题型（排除上一题型）
```

| 实体 | 用途 |
|------|------|
| `learning_sets` | 当前学习集 |
| `learning_sessions` | 练习会话 |
| `learning_attempts` | 单题作答 |
| `word_mastery` | 掌握度 |

---

## 6. 词库

| 类型 | 说明 |
|------|------|
| **标准词库** | 四级难度，词性/音标/例句可编辑，TTS 可校正 |
| **非标准词库** | 词典拉取 + TTS 自动生成 |
| **班级 / 教师词表** | 引用标准库或自定义 |

**文件夹分类：** 通用（年级/学期/单元）、雅思/托福、专业（化学/经济/物理…）

**排序：** 教材序（默认）、字母序、COCA 词频

**TTS 流水线（异步）：**

```
词项 → 词典 API → 科大讯飞 TTS → OSS → 记录 audio_url → 后台可校正
```

---

## 7. 权限模型

### 班级结构

```
Class（班级）
  ├── 教师（可多个）
  └── 学员（用户）
```

### 角色

| 角色 | 范围 | 约束 |
|------|------|------|
| `admin` | 全系统 | 班级、词库、题库 |
| `teacher` | 自己的班 | 不见学员联系方式；报告仅含自己布置的词表 |
| `user` | 本人数据 + 所属班级内容 | 未绑班则仅个人词表与学习集 |

### 实现

- JWT：`sub`, `roles[]`, `class_ids[]`（或查库推导）
- service 层校验班级范围，不只靠 handler
- 按角色过滤 DTO 敏感字段
- 标准词库与班级词表通过引用关联

---

## 8. 数据模型（简化 ER）

```
Class ──< ClassUser >── User
  │
  ├── role: teacher | member
  └──< ClassWordList >── WordList

WordList ──< WordListItem >── Word ──< WordSense / WordAudio
  │
  ├── scope: standard | class | teacher
  └── folder, sort_mode

LearningSet ──< LearningSession ──< LearningAttempt

QuestionBank ──< Question
```

**索引关注：**

- `class_users(class_id, user_id)`
- `learning_attempts(user_id, created_at)`
- `word_list_items(word_list_id, sort_order)`
- `word_lists(scope, owner_id)`

---

## 9. 前端建议

| 端 | 技术方向 |
|----|----------|
| 学员 App | Flutter / RN / Uni-app |
| 教师 / 管理后台 | React + Ant Design 或 Vue + Element Plus |
| 落地页 | Next.js / 静态站 |

---

## 10. 外部集成

| 集成 | 方式 |
|------|------|
| 词典 API | `integration/dictionary` |
| COCA 词频 | 导入 PostgreSQL |
| 科大讯飞 TTS | 异步任务 + 重试 |
| 对象存储 | 预签名 URL |

---

## 11. 部署

**当前：** Docker Compose（app + postgres + migrate）

**生产演进：** LB → tsz-go × N → PostgreSQL（+ 副本）/ Redis / OSS

- 日志：`slog` JSON
- 迁移：`cmd/migrate` 独立执行，生产不用 `AUTO_MIGRATE=true`

---

## 12. 实施路线

### Phase 1 — 基础

- [x] 用户注册登录、JWT
- [ ] 角色 RBAC + 班级范围
- [ ] 班级 CRUD、用户绑班
- [ ] 基础词表 CRUD

### Phase 2 — 学习闭环

- [ ] 学习集、调度器
- [ ] 闪卡 / 判断 / 选择 / 拼写
- [ ] 学习记录与统计
- [ ] 教师进度报告

### Phase 3 — 内容

- [ ] 标准词库 CMS
- [ ] TTS pipeline + 音频校正
- [ ] 题库
- [ ] 会员（未绑班用户）

### Phase 4 — 规模化

- [ ] Redis + asynq
- [ ] 读副本、报表导出

---

## 13. 与现有代码衔接

1. `make migrate-create name=...`
2. 新建 `internal/<domain>/`（参考 `internal/user/`）
3. `cmd/server/main.go` 装配
4. `router.go` 注册路由
5. service 层做权限与班级范围校验

Repository 可后续换 [sqlc](https://sqlc.dev)，service 接口不变。

---

## 14. 总结

| 项 | 结论 |
|----|------|
| 架构 | 模块化单体 + PostgreSQL |
| 层级 | **仅班级** |
| 角色 | `admin` / `teacher` / `user` |
| 核心难点 | 学习调度引擎、词库 CMS + TTS |
