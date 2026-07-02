# 智能词库模块设计 v2 —— 表结构与创建单词接口（定稿）

> 状态：**定稿（2026-07-02 评审通过）**。D1–D17 全部拍板：D1/D2/D3/D9/D10 逐条确认（行内
> 标 ✅ 并含补充口径），其余整体确认"没啥问题"。本文取代 `feat/wordlist` 分支上的评审稿 v1，
> 是该模块唯一事实来源；按 §9 分段落地，并同步 api.md / openapi.yaml / architecture.md。
>
> 范围：**表结构（可直接落迁移的 DDL）+ 创建单词接口（建壳 / 全量保存 / 发布 / 聚合读）**。
> 语音合成、三方词频、自动出题、审核流、例句库对接均为**预留**，只留字段与扩展点，不在本期实现。
>
> 需求事实来源：产品交互稿三步表单（基本信息 → 详细信息 → 完成创建）+ 26 条批注（`[n]` 标注），
> 列表页交互稿（创建单词/创建短语、草稿/已发布、发布后生成或更新题），以及 Admin 端功能结构
> 白板图（[whiteboard_exported_image.png](../../docs/whiteboard_exported_image.png)，下称
> `[白板]`——覆盖：分页列表操作、创建单词全字段树、创建短语、查询条件六项）。

---

## 1. 定位

- 管理员在后台为一个**词条**（单词或短语）录入全部教学内容：方言、词性、词形变化、发音、
  语法结构、多维词义/释义/例句、关联词。属内容创作（CMS），是学习/出题系统的**内容中枢**。
- 学生侧做题、学习进度不在本模块；但出题规则决定本模块必须存哪些字段（见 §8）。
- 全部接口挂 `/api/v1/admin/words`，走 `AdminAuthRequired`（realm=admin）。

---

## 2. 决策清单（审核重点，逐条拍板）

> 每条给出建议与依据。**编号 D1–D17**，审核时按编号反馈即可。带（Q*n*）的对应 v1 待确认问题。

| # | 决策点 | 建议 | 依据 / 影响 |
|---|--------|------|-------------|
| **D1** | 单词与短语同表 | **✅ 已拍板（2026-07-02）**：`words.kind ∈ {word, phrase}`，共用整棵子树与列表/接口 | 列表页同时有"创建单词/创建短语"，同列表混排、同状态机。`[白板]` 中"创建短语"是独立分支但**无任何子结构**——短语表单未细化，暂按同一棵树处理；若有校验差异，发布校验按 kind 分支，表结构不变 |
| **D2** | headword 唯一性（Q11） | **✅ 已拍板（2026-07-02）**：`UNIQUE (lower(headword), kind)`，一期不允许同形多词条 | 简单、防重复录入；真遇到同形多词条（同拼写不同词源）再放开，放开是减约束不伤存量 |
| **D3** | 词义等级：手选（Q1） | **✅ 已拍板（2026-07-02）**：`word_senses.level` 为**必填输入列**，不做服务端派生。后台只负责录入多个等级的词义并存储；"学生看不到高于自身等级的词义"是 **C 端/学习系统消费时**的过滤逻辑，本模块无特殊处理 | 交互稿是必填可编辑下拉 `[25]`；批注 `[11]`"取最低等级释义语句的等级"仅作**前端默认填充**建议。列表页"难度"列 = 各词义 level 去重聚合，读时算 |
| **D4** | 语法结构挂词性、两层结构（Q2） | 挂 `word_pos`，不跨词性共享。两层：`结构组`（编号 1/2/3）+ `方言行`（英/美/通用各一行）；**释义引用结构组** | 交互稿中语法结构在词性 tab 内；同一编号下英美成对录入 `[1]`；释义与方言无关（中/英释义 `[9]`），故引用组、渲染时按方言取行 |
| **D5** | 多维例句弱引用（Q3） | `source_example_id UUID` 无 FK + `text` 快照冗余 | 侧边栏存在独立"多维例句"模块（未建）。弱引用 + 快照使本模块不被例句库阻塞；对接后由例句库回填/同步 |
| **D6** | 关联词引用方式（Q4） | 只能选**已入库**词条的词义；`target_word_id`/`target_sense_id` FK `ON DELETE SET NULL` + `target_headword`/`target_gloss` 快照。`target_gloss` 取目标词义**第一条中文释义**（def_type=zh），无中文则退第一条 | 交互稿流程是"搜索已有词 → 选词义" `[17]`；`[白板]` 近义/反义/派生 item 字段明确为"近似度\|对立度\|关联度 / 单词 / **中文释义**"。目标被删后快照仍可展示，行不悬空报错 |
| **D7** | 富文本 JSON schema v1（Q5） | `{version, text, spans[{start,end,type∈bold\|blue}], liaisons[int]}`，见 §5 | 蓝色/加粗需喂语音合成与训练定位 `[19]`，纯 HTML 不可用；连读符是**字母间位置**标记，独立于 spans |
| **D8** | 可配置枚举不落 CHECK（Q6） | `pos`/`form_type`/`sub_pos`：Go 侧种子枚举校验、DB 只 TEXT 不 CHECK；`dialect`/`status`/`style` 等稳定枚举落 DB CHECK | 前者产品规划为"后台单词词形配置"可配置项——将来配置化只改代码不动迁移；后者语义稳定，DB 兜底 |
| **D9** | 词频（Q7） | **✅ 已拍板（2026-07-02）**：词频由**维护者手动录入**，后端只存值、**无额外逻辑**（不做三方拉取）。`NUMERIC(12,6)`，百分比数值（UI 显示 `0.023134 %`），草稿可空、发布必填。`word_senses.frequency` 默认=词条词频（**前端填充**，服务端不隐式复制） | 批注 `[5]` 的"自动从三方获取"经确认不做；可空+后补不阻塞录入。词义词频出题排序要用 `[12]`，必须独立成列 |
| **D10** | 音频只留字段（Q8） | **✅ 已拍板（2026-07-02）**：TTS 生成与上传语音**后续开发**，本期只留字段——各可发声实体带 `audio_url TEXT` + `audio_source ∈ {tts, upload}`，生成/上传接口不实现 | TTS 与 OSS 均未开通（同 user-module 头像阻塞项）。字段在位，后接预签名直传 + TTS 弹框参数 `[15][22][27]`；`[白板]` 在词形发音 item、语法结构、多维释义上均标"发音 / 上传语音 / 获取语音"三操作，与留字段的实体范围一致 |
| **D11** | 状态机与出题触发（Q9/Q10） | `status ∈ {draft, published}`；`POST …/publish` 强校验后置为 published。发布成功后调用**空实现 hook**（出题触发预留）。status 为 TEXT，后续加审核态（如 `in_review`）只改代码 | "保存/提交"两个按钮 ⟹ 草稿宽校验、发布强校验；列表页批注"增加审核机制，发布后才会生成或更新题" |
| **D12** | 关联度 0–100 整数（Q12） | `score SMALLINT CHECK (0..100)`，默认 **0** | UI 录入 `87 %` 整数百分比；默认 0 以批注 `[2]` 为准（交互稿 87 是示例值） |
| **D13** | 权限（Q13） | `AdminAuthRequired` 即可创作，一期不细分内容编辑角色 | 现有 admin 体系只有 admin/super_admin 两级；`[白板]` 中"超级管理员"与"智能词库"是 Admin 端下**并列分支**，词库功能未挂在超管之下——普通 admin 可创作的口径成立 |
| **D14** | 接口形态：建壳 + 整树全量保存 | `POST /words` 建壳 → `PUT /words/{id}/content` **整树全量保存** → `POST /words/{id}/publish`；读用 `GET /words/{id}` 聚合。**不做子资源级 CRUD** | 三步表单 + "保存"按钮语义就是全量提交；单词条树有界（几 KB～几十 KB）。子资源 CRUD 会让前端维护 diff、一次保存发 N 个请求且无事务性 |
| **D15** | 树节点 id 由客户端生成 | 树内全部节点 id 为 UUID，**新建节点由前端生成**；保存时服务端按 id diff-upsert（有→update，无→insert，缺→delete） | 两个硬需求：① 同一次保存里"释义 → 语法结构组"等**树内引用**需要 id；② `word_senses.id` 被其他词条的关联词引用，**必须跨保存稳定**，delete+reinsert 会打断跨词条引用 |
| **D16** | 审计与并发 | `created_by`/时间戳只落 `words` 主表，子表不带；保存体带 `base_updated_at` 乐观锁，不匹配返 409 | 整树保存下子表行随保存整体演进，词条级审计已够。多管理员同时编辑同一词条时防丢改（列表页可见多个创建人协作） |
| **D17** | 已发布词条的编辑 | published 词条可直接 `PUT …/content`，但按**发布级强校验**（不允许改残），保存即生效并触发"更新题"hook（本期空实现） | 列表页对已发布词条有"编辑"入口；批注"发布后才会生成**或更新**题"说明发布后内容可变且要联动出题 |

---

## 3. 领域模型总览

一个词条是一棵树。`*` 发布时必填，`?` 选填，`[n]` 为批注号。

```
word（词条）* — kind: 单词|短语 (D1)
├─ frequency  词频* [5]（[白板] 称"总词频"；百分比，6 位小数，草稿可空）(D9)
├─ dialect_mode  区分/不区分方言* [3]；区分时 dialects ⊆ {uk,us} 且非空 [21]
├─ sense_groups  语义区间? [4]（0..n 命名分组，供词义单选引用）
└─ pos  基本词性* [23]（1..n；以下全部从属于某个 pos）
   ├─ forms  词形变化* [6]
   │   ├─ dialect：区分→英/美各一套；不区分→common（"通用"）[21]
   │   ├─ form_type 同一类别可重复（可以有两个过去式）[6]；base（原形）每方言恰一条、不可删 [6]
   │   └─ pronunciations  发音* [6]（1..n/词形）
   │       └─ 字典音标* / 实际发音* / 发音方式*(normal|strong|weak，默认 normal) / 音频? (D10)
   ├─ grammar_structures  语法结构* [1]（结构组 1..n）(D4)
   │   └─ variants：每个已勾方言（或 common）一行，富文本 (D7) + 音频?
   └─ senses  词义* [11]（1..n/pos，id 跨保存稳定 (D15)）
      ├─ sub_pos 细分词性* [16] / level 词义等级* CEFR (D3) / sense_group? [8]
      ├─ frequency 词义词频*（[白板] 称"语义词频"）[12] / depends_on_context 是否依赖语境* [7]
      ├─ definitions  多维释义* [9]（1..n）：level CEFR / type 中文|英语 / 富文本 / →语法结构组? / 音频?
      ├─ sentences  多维例句? [10]（0..n）：例句库弱引用 (D5) + 文本快照 + 音频?
      └─ relations  关联词? [2]：近义|反义|派生 / →目标词条的词义 (D6) / score 0..100 (D12)
                    / 中文释义快照（[白板]，取目标词义第一条 zh 释义）
```

**派生展示字段（不落库，读时算）**：词义 tab 标题 = 第一条释义前 10 字符 `[20]`；列表页
"释义"列 = 第一个词性第一个词义的第一条释义；"难度"列 = 全部词义 level 去重排序。

---

## 4. 表结构定稿（迁移 `000018_create_words`）

> 迁移号说明：v1 评审稿写的下一号 000017 已被 `feat/coin-ledger` 分支的
> `000017_create_coin_wallet_ledger` 占用（共享测试库已应用），words 顺延为 000018。
> **合并顺序约束**：golang-migrate 不回头应用低于当前版本的迁移——必须让 coin(17) 先于
> words(18) 合入 main；若 words 先合，coin 需改号到 19。

> 风格沿用现有迁移：UUID 主键应用层生成、TEXT + CHECK（稳定枚举）、`TIMESTAMPTZ DEFAULT now()`。
> 有序列表统一 `sort_order INT NOT NULL`（服务端按保存体数组序回写）。11 张表。

### 4.1 words — 词条主表

```sql
CREATE TABLE words (
    id           UUID        PRIMARY KEY,
    kind         TEXT        NOT NULL DEFAULT 'word'
                 CHECK (kind IN ('word', 'phrase')),                    -- D1
    headword     TEXT        NOT NULL,
    frequency    NUMERIC(12,6),          -- 百分比值；草稿可空，发布必填 (D9)
    dialect_mode TEXT        NOT NULL DEFAULT 'unified'
                 CHECK (dialect_mode IN ('distinguish', 'unified')),    -- [3]
    dialects     TEXT[]      NOT NULL DEFAULT '{}',  -- distinguish 时 ⊆ {uk,us} 且非空；unified 时空
    status       TEXT        NOT NULL DEFAULT 'draft'
                 CHECK (status IN ('draft', 'published')),              -- D11（TEXT 便于后续加审核态）
    created_by   UUID        NOT NULL REFERENCES admins (id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()   -- 整树任何变更都 bump，兼作乐观锁 (D16)
);

CREATE UNIQUE INDEX words_headword_kind_unique ON words (lower(headword), kind);  -- D2
CREATE INDEX words_status_idx     ON words (status);
CREATE INDEX words_created_at_idx ON words (created_at DESC);   -- 列表默认排序
```

### 4.2 word_sense_groups — 语义区间 `[4]`

```sql
CREATE TABLE word_sense_groups (
    id         UUID PRIMARY KEY,
    word_id    UUID NOT NULL REFERENCES words (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    sort_order INT  NOT NULL
);
CREATE INDEX word_sense_groups_word_idx ON word_sense_groups (word_id);
```

### 4.3 word_pos — 基本词性 `[23]`

```sql
CREATE TABLE word_pos (
    id         UUID PRIMARY KEY,
    word_id    UUID NOT NULL REFERENCES words (id) ON DELETE CASCADE,
    pos        TEXT NOT NULL,   -- verb/noun/pronoun/adjective/...，Go 种子枚举校验，不落 CHECK (D8)
    sort_order INT  NOT NULL,
    UNIQUE (word_id, pos)       -- 同一词条同词性只一个 tab
);
CREATE INDEX word_pos_word_idx ON word_pos (word_id);
```

一期 `pos` 种子枚举（对齐列表页徽标）：`noun(n.) pronoun(pron.) verb(v.) adjective(adj.)
adverb(adv.) preposition(prep.) article(art.) determiner(det.) conjunction(conj.)
numeral(num.) interjection(int.)`。

### 4.4 word_forms / word_form_pronunciations — 词形与发音 `[6][21]`

```sql
CREATE TABLE word_forms (
    id          UUID PRIMARY KEY,
    word_pos_id UUID NOT NULL REFERENCES word_pos (id) ON DELETE CASCADE,
    dialect     TEXT NOT NULL CHECK (dialect IN ('uk', 'us', 'common')),  -- 不区分→common [21]
    form_type   TEXT NOT NULL,   -- base/present_participle/past_tense/...，Go 种子枚举 (D8)
                                 -- 同类可重复（两个过去式合法 [6]），故无唯一约束；
                                 -- base 每 (pos,dialect) 恰一条 → 发布校验保证（§7）
    spelling    TEXT NOT NULL DEFAULT '',
    sort_order  INT  NOT NULL
);
CREATE INDEX word_forms_pos_idx ON word_forms (word_pos_id);

CREATE TABLE word_form_pronunciations (
    id            UUID PRIMARY KEY,
    word_form_id  UUID NOT NULL REFERENCES word_forms (id) ON DELETE CASCADE,
    dict_phonetic TEXT NOT NULL DEFAULT '',   -- 字典音标；草稿可空串，发布必填
    actual_pron   TEXT NOT NULL DEFAULT '',   -- 实际发音
    style         TEXT NOT NULL DEFAULT 'normal'
                  CHECK (style IN ('normal', 'strong', 'weak')),          -- 默认正常 [6]
    audio_url     TEXT,                                                   -- D10 预留
    audio_source  TEXT CHECK (audio_source IN ('tts', 'upload')),
    sort_order    INT  NOT NULL
);
CREATE INDEX word_form_pronunciations_form_idx ON word_form_pronunciations (word_form_id);
```

一期 `form_type` 种子枚举（动词/名词/形容词常用集，随"词形配置"模块扩展）：`base(原形)
present_participle(现在分词) past_tense(过去式) past_participle(过去分词)
third_person_singular(第三人称单数) plural(复数) comparative(比较级) superlative(最高级)`。

### 4.5 语法结构：结构组 + 方言行 `[1][19]`（D4）

```sql
-- 编号组：释义引用的目标，与方言无关
CREATE TABLE word_grammar_structures (
    id          UUID PRIMARY KEY,
    word_pos_id UUID NOT NULL REFERENCES word_pos (id) ON DELETE CASCADE,
    sort_order  INT  NOT NULL          -- 即交互稿里的编号 1/2/3
);
CREATE INDEX word_grammar_structures_pos_idx ON word_grammar_structures (word_pos_id);

-- 方言行：区分方言时英/美各一行，不区分时 common 一行
CREATE TABLE word_grammar_structure_variants (
    id           UUID PRIMARY KEY,
    structure_id UUID  NOT NULL REFERENCES word_grammar_structures (id) ON DELETE CASCADE,
    dialect      TEXT  NOT NULL CHECK (dialect IN ('uk', 'us', 'common')),
    content      JSONB NOT NULL,       -- 富文本 v1（§5）
    audio_url    TEXT,
    audio_source TEXT CHECK (audio_source IN ('tts', 'upload')),
    UNIQUE (structure_id, dialect)
);
```

### 4.6 word_senses / definitions / sentences — 词义、释义、例句 `[7]–[12][16]`

```sql
CREATE TABLE word_senses (
    id                 UUID PRIMARY KEY,   -- 被其他词条 relations 引用，跨保存稳定 (D15)
    word_pos_id        UUID NOT NULL REFERENCES word_pos (id) ON DELETE CASCADE,
    sub_pos            TEXT NOT NULL DEFAULT '',   -- 细分词性 V-T 等，Go 种子枚举 (D8)
    level              TEXT NOT NULL
                       CHECK (level IN ('A1','A2','B1','B2','C1','C2')), -- 手选 (D3)
    sense_group_id     UUID REFERENCES word_sense_groups (id) ON DELETE SET NULL, -- 单选可空 [8]
    frequency          NUMERIC(12,6),              -- 默认=词条词频，可改 [12]；发布必填
    depends_on_context BOOLEAN NOT NULL DEFAULT false,   -- [7]，决定题型（§8）
    sort_order         INT  NOT NULL
);
CREATE INDEX word_senses_pos_idx ON word_senses (word_pos_id);

CREATE TABLE word_sense_definitions (        -- 多维释义 [9]
    id                   UUID PRIMARY KEY,
    sense_id             UUID  NOT NULL REFERENCES word_senses (id) ON DELETE CASCADE,
    level                TEXT  NOT NULL CHECK (level IN ('A1','A2','B1','B2','C1','C2')),
    def_type             TEXT  NOT NULL CHECK (def_type IN ('zh', 'en')),  -- 中文/英语释义
    text                 JSONB NOT NULL,     -- 富文本 v1（§5）
    grammar_structure_id UUID REFERENCES word_grammar_structures (id) ON DELETE SET NULL, -- 引用结构组 (D4)
    audio_url            TEXT,
    audio_source         TEXT CHECK (audio_source IN ('tts', 'upload')),
    sort_order           INT   NOT NULL
);
CREATE INDEX word_sense_definitions_sense_idx ON word_sense_definitions (sense_id);

CREATE TABLE word_sense_sentences (          -- 多维例句 [10]
    id                UUID PRIMARY KEY,
    sense_id          UUID NOT NULL REFERENCES word_senses (id) ON DELETE CASCADE,
    source_example_id UUID,                  -- 例句库弱引用，无 FK (D5)
    text              JSONB,                 -- 快照/手填富文本
    audio_url         TEXT,
    audio_source      TEXT CHECK (audio_source IN ('tts', 'upload')),
    sort_order        INT  NOT NULL
);
CREATE INDEX word_sense_sentences_sense_idx ON word_sense_sentences (sense_id);
```

### 4.7 word_sense_relations — 关联词 `[2][17][18]`（D6/D12）

```sql
CREATE TABLE word_sense_relations (
    id              UUID PRIMARY KEY,
    sense_id        UUID NOT NULL REFERENCES word_senses (id) ON DELETE CASCADE, -- 本词条侧
    relation        TEXT NOT NULL CHECK (relation IN ('synonym', 'antonym', 'derivative')),
    target_word_id  UUID REFERENCES words (id)       ON DELETE SET NULL,  -- 目标词条
    target_sense_id UUID REFERENCES word_senses (id) ON DELETE SET NULL,  -- 目标词义 [17]
    target_headword TEXT NOT NULL,                   -- 快照：目标被删后仍可展示
    target_gloss    TEXT NOT NULL DEFAULT '',        -- 快照：目标词义第一条中文(zh)释义，
                                                     -- 无中文退第一条 [17][白板]
    score           SMALLINT NOT NULL DEFAULT 0 CHECK (score BETWEEN 0 AND 100), -- D12
    sort_order      INT  NOT NULL
);
CREATE INDEX word_sense_relations_sense_idx  ON word_sense_relations (sense_id);
CREATE INDEX word_sense_relations_target_idx ON word_sense_relations (target_sense_id);
```

> 一期近义词参与做题展示；反义/派生可录但不参与业务 `[2]`——数据层不区分，业务层过滤。

---

## 5. 富文本 JSON schema v1 `[19]`（D7）

释义 `text`、语法结构 `content`、例句 `text` 共用同一 schema：

```json
{
  "version": 1,
  "text": "This is an English sentence.",
  "spans":    [ { "start": 8,  "end": 10, "type": "bold" },
                { "start": 11, "end": 18, "type": "blue" } ],
  "liaisons": [ 4, 12 ]
}
```

- `text`：纯文本，UTF-8，偏移按 **Unicode 码点**（服务端校验用 rune 数）。**前端注意**：JS
  字符串索引是 UTF-16 code unit，含 emoji 等星形平面字符时必须换算成码点（`[...str]` 或
  `Intl.Segmenter`），否则标注区间会整体错位——越界会被 400 拒，界内错位则静默标错。
- `spans`：左闭右开 `[start, end)`；`type ∈ {bold, blue}`。`bold`/`blue` 需喂语音合成；
  `blue` 另供训练侧定位"学生需完成的部分"（填空/跟读重点）。允许重叠。
- `liaisons`：连读符位置，`i` 表示"第 `i` 与 `i+1` 个码点之间"（0-based），仅展示用。
- "清除格式" = 前端行为（spans/liaisons 置空），不需要后端语义。
- 省略/传 null 的 `spans`/`liaisons` 服务端归一化为 `[]`，存储与回读均不为 null。
- 服务端校验：offset 不越界、start < end、type/结构合法、text ≤5000 码点、每类标注 ≤500 条；
  其余宽进。

---

## 6. 创建单词接口（本期交付的核心契约）

> 前缀 `/api/v1/admin/words`，全部 `AdminAuthRequired`。错误体沿用 `{ "error": "<message>" }`；
> 发布校验失败追加 `details` 数组（见 6.3）。列表分页沿用 `page/page_size` + `"page"` 响应体。

### 6.0 总览

| 方法 | 路径 | 说明 | 本期 |
|------|------|------|:---:|
| POST | `/admin/words` | **建壳**（第一步"基本信息"）| ✅ |
| PUT | `/admin/words/{id}/content` | **整树全量保存**（"保存"按钮，D14/D15）| ✅ |
| POST | `/admin/words/{id}/publish` | **发布**（"提交"按钮，强校验，D11）| ✅ |
| GET | `/admin/words/{id}` | **聚合读**整棵树（编辑页回显 `[20]`）| ✅ |
| GET | `/admin/words` | 列表（分页 + 筛选）| ✅ |
| GET | `/admin/words/stats` | 累计/今日/本月创编计数（列表页头）| ✅ |
| DELETE | `/admin/words/{id}` | 删词条（树级联；他词条关联词 SET NULL）| ✅ |
| POST | `/admin/words/batch-delete` | 批量删（列表页复选框）| ✅ |
| GET | `/admin/words/related-search?q=&kind=` | 关联词弹框：搜词条并列其词义 `[17]` | ✅ |
| POST | `…/audio/generate`、`…/audio/upload` | TTS / 上传（D10）| ⏳ 后续 |
| GET | `…/config/…` | 词形/词性配置下拉源（D8 配置化后）| ⏳ 后续 |

### 6.1 建壳 `POST /admin/words`

```json
// 请求
{ "headword": "centre", "kind": "word" }
// 201
{ "word": { "id": "…", "headword": "centre", "kind": "word", "status": "draft",
            "frequency": null, "dialect_mode": "unified", "dialects": [],
            "created_at": "…", "updated_at": "…" } }
```

- 校验：headword 非空（trim 后）、kind 合法；`409 {"error": "word already exists"}` 撞唯一索引。
- 词频不在建壳时产生：维护者在第二步表单手动录入，随 `PUT …/content` 落库（D9 已拍板，
  不做三方拉取）。

### 6.2 整树全量保存 `PUT /admin/words/{id}/content`

"保存"按钮语义：前端把整棵树（第二步表单全量状态）一次提交，服务端**单事务**按 id
diff-upsert（D14/D15）。**新建节点的 id 由前端生成 UUID**；保存体里未出现的既有子节点视为删除。

```json
{
  "base_updated_at": "2026-07-02T10:00:00Z",        // 乐观锁 (D16)
  "frequency": "0.023134",
  "dialect_mode": "distinguish",
  "dialects": ["uk", "us"],
  "sense_groups": [ { "id": "g1…", "name": "空间定位的动作" } ],
  "pos": [
    {
      "id": "p1…", "pos": "verb",
      "forms": [
        { "id": "f1…", "dialect": "uk", "form_type": "base", "spelling": "centre",
          "pronunciations": [
            { "id": "pr1…", "dict_phonetic": "ˈsentə", "actual_pron": "ˈsentə", "style": "strong" },
            { "id": "pr2…", "dict_phonetic": "ˈsentə", "actual_pron": "ˈsentə", "style": "weak" } ] },
        { "id": "f2…", "dialect": "uk", "form_type": "past_tense", "spelling": "centred",
          "pronunciations": [ { "id": "pr3…", "dict_phonetic": "ˈsentəd",
                                "actual_pron": "ˈsentəd", "style": "normal" } ] }
      ],
      "grammar_structures": [
        { "id": "gs1…", "variants": [
            { "dialect": "uk", "content": { "version": 1, "text": "…", "spans": [], "liaisons": [] } },
            { "dialect": "us", "content": { "version": 1, "text": "…", "spans": [], "liaisons": [] } } ] }
      ],
      "senses": [
        { "id": "s1…", "sub_pos": "V-T", "level": "A1", "sense_group_id": "g1…",
          "frequency": "0.023134", "depends_on_context": false,
          "definitions": [
            { "id": "d1…", "level": "A1", "def_type": "zh",
              "text": { "version": 1, "text": "使其居中…", "spans": [], "liaisons": [] },
              "grammar_structure_id": "gs1…" } ],
          "sentences": [
            { "id": "ms1…", "source_example_id": null,
              "text": { "version": 1, "text": "…centre the picture on the wall.",
                        "spans": [], "liaisons": [] } } ],
          "relations": [
            { "id": "r1…", "relation": "synonym", "target_word_id": "w2…",
              "target_sense_id": "s9…", "score": 87 } ]
        }
      ]
    }
  ]
}
```

要点：

- **数组序即 sort_order**，body 不带显式序号；关联词快照（`target_headword`/`target_gloss`）
  由**服务端**在保存时从目标词条抄录（gloss 取目标词义第一条 zh 释义，无则第一条，D6），
  body 不传、传也忽略。
- **音频字段不走此接口**：`audio_url/audio_source` 由后续生成/上传接口维护，全量保存不触碰
  既有音频（按 id upsert 时保留原值），避免"保存一次丢全部语音"。
- 响应 `200` 返回与 6.4 聚合读相同的整树（含服务端回写的 `updated_at`）。

**保存校验（草稿宽校验——结构对、引用对，不管全不全）：**

1. 词条存在且未删；`base_updated_at` 与当前 `updated_at` 不符 → `409 {"error":"word was modified by others"}`。
2. **id 归属**：所有带 id 的既有节点必须属于本词条（防跨词条覆盖）；否则 400。
3. 枚举合法：dialect 与 `dialect_mode/dialects` 一致（distinguish → 只允许勾选的方言；
   unified → 恒 common）`[3][21]`；pos/form_type/sub_pos 落在种子枚举内；level/def_type/
   relation/style 合法。
4. 树内引用闭合：`sense_group_id` ∈ 本词条 groups；`grammar_structure_id` ∈ **同 pos** 的结构组；
   `target_sense_id` 属于 `target_word_id`（且目标词条存在）。
5. 富文本 schema 校验（§5）；`score ∈ [0,100]`；`frequency ∈ [0,100]`。
6. published 词条：在上述之外**叠加发布级完整性校验**（D17），不允许改残。

### 6.3 发布 `POST /admin/words/{id}/publish`

- 通过 → `200 {"word": {…, "status": "published"}}`，并调用出题触发 hook（本期空实现，D11）。
- 完整性校验失败 → `422`：

```json
{ "error": "word is incomplete",
  "details": [ "frequency is required",
               "pos verb: missing base form for dialect uk",
               "pos verb, sense 1: at least one definition is required" ] }
```

**发布完整性校验清单（§7 汇总）**、重复发布幂等（published 再 publish → 200 直接返回，
重新跑一次 hook 以支持"更新题"）。

### 6.4 聚合读 `GET /admin/words/{id}`

返回整棵树，结构 = 6.2 保存体 + 服务端字段（`id/kind/headword/status/created_by/created_at/
updated_at`、各节点 `audio_url/audio_source`、关联词快照）。前端编辑页一次拉全、tab 间切换
不再请求 `[20]`。实现：单事务按 word_id 分表批量查（≈8 条索引查询），内存组树。

### 6.5 列表与统计（列表页）

```
GET /admin/words?page=1&page_size=20&q=&gloss=&kind=&pos=&level=&status=&created_from=&created_to=
```

- 筛选六项对齐 `[白板]` 查询分支（关键字模糊/释义/类型/基本词性/难度/创建时间）；`status`
  为白板之外低成本附送。`q`：headword 前缀/子串 或 创建人名（列表页关键字框提示"词汇/创建
  人"）；`gloss`：任意释义文本子串（`text->>'text' ILIKE`）；`pos`/`level`：EXISTS 子查询命
  中任一词性/任一词义等级。
- 响应行：`id, headword, kind, gloss(第一词义第一条释义), pos_list(徽标), levels(难度列),
  status, created_by_name, created_at, updated_at` + `"page"` 分页体。释义/难度列读时聚合
  （LATERAL）；量级（千级词条）下无需反规范化，慢了再加汇总列。
- `GET /admin/words/stats` → `{ "total": 234, "today": 24, "month": 124 }`（按 created_at，
  Asia/Shanghai 口径）。

---

## 7. 发布完整性校验清单（强校验，publish 与 published-save 共用）

| # | 规则 | 出处 |
|---|------|------|
| V1 | `frequency` 非空 | `[5]` |
| V2 | `dialect_mode=distinguish` → `dialects` 非空 ⊆ {uk,us}；`unified` → 空 | `[3]` |
| V3 | ≥1 个 `pos` | `[23]` |
| V4 | 每 pos × 每个已勾方言（unified 则 common）：**恰好 1 条** `form_type=base` 的词形 | `[6]` |
| V5 | 每词形 `spelling` 非空、≥1 条发音；每发音 `dict_phonetic`/`actual_pron` 非空 | `[6]` |
| V6 | 每 pos ≥1 个语法结构组；每组在每个已勾方言下都有 variant 且 `text` 非空 | `[1]` |
| V7 | 每 pos ≥1 个词义；每词义 ≥1 条释义 | `[11]` |
| V8 | 每词义 `sub_pos` 非空、`frequency` 非空 | `[16]` `[12]` |
| V9 | 每条释义 `text` 非空 | `[9]` |
| V10 | 关联词行必须已选词义（`target_sense_id` 非空）——对应"请先选择词义" | `[17]` |

草稿态以上全部放宽（可缺可残），只做 6.2 的结构/引用/枚举校验。

---

## 8. 出题规则 ⇒ 字段依据（规则固化，出题不在本模块）

出题在学习系统，发布/更新时经 hook 通知（D11/D17）。规则决定三组必存字段：

1. **`depends_on_context`** `[7]`：跟读/翻译恒出；听写/拼写/多选**仅不依赖语境**出（用语法
   结构）；单选两种都出但素材切换（不依赖→释义绑定的语法结构；依赖→符合学生等级的例句）。
2. **CEFR 可比较**（`word_senses.level`、`definitions.level`）`[25]`：词义等级 ≤ 学生等级才学；
   出题取与学生同级的释义/例句，无同级取最接近。**C 端查看词义同样受此门槛约束**（等级高于
   学生的词义不可见）——过滤全在消费侧，本模块只保证 level 存储且可比较（D3 已拍板）。
   CEFR 序数映射在 Go 侧（A1=1 … C2=6）。
3. **`word_senses.frequency`** `[12]`：题目携带词义词频，决定出题顺序。

---

## 9. 实现分段（✅ 三段已全部落地，2026-07-02）

> 实现在 `internal/word/`（纵切：model / repository / service / handler +
> fake/contract/service/handler/integration 五类测试），迁移 `000018`，路由挂
> `/api/v1/admin/words`（router.go），api.md 与 openapi.yaml（tag `Admin (words)`）
> 已同步。`sub_pos` 同样走 D8 种子枚举（2026-07-02 拍板：前后端共用一份映射，
> 后端存 code、前端显示中文标签），清单见 model.go `SubPOSValues` 与 openapi
> `WordSense.sub_pos`：V-T/V-I/V-LINK/AUX/MODAL、N-COUNT/N-UNCOUNT/N-PROPER/
> N-PLURAL/N-SING、ADJ/ADV/PRON/PREP/CONJ/DET/ART/NUM/INT。

1. **第一段 · 迁移 + 领域内核**：`000018` 迁移、model、repository（含 diff-upsert 树保存）、
   fake + Store 契约测试（fake==真库）、发布校验器单测。
2. **第二段 · HTTP 装配**：handler（建壳/保存/发布/聚合读/列表/统计/删除/关联搜索）、
   handler 测试、路由挂载、api.md + openapi.yaml 同步。
3. **第三段 · 集成测试**：真库全链路（建壳→保存→发布→跨词条关联→删除级联），遵守集成测试
   铁律（数据全局唯一、不清库、无进程内计数器）。

后续（不阻塞本期）：TTS + OSS 音频（D10）、审核态（D11）、出题事件真实装配、例句库/词形
配置模块对接（D5/D8）。三方词频经确认**不做**（D9，手动录入）。

### 9.1 深度评审修订（2026-07-02，10 视角审查后按批次修复）

语义上区别于初版实现的收口，均已落码并有契约/单测钉住：

1. **孤儿关联词（D6 补全）**：目标词条被删后 FK 置 NULL、快照保留的行是**合法回存输入**——
   保存时服务端**冻结**其四个 target 列（如同音频列），仅行自身字段可改；新建行仍必须带
   `target_word_id`。V10 只对「目标存活但未选词义」报违规，孤儿行不阻塞发布。
2. **发布乐观锁（D16 延伸）**：`SetStatus` 带 base updated_at 前置条件（不匹配 → 409），
   杜绝「校验通过后、置态前」被并发保存改残的 TOCTOU。
3. **可延迟唯一约束**：`word_pos(word_id,pos)` 与变体 `(structure_id,dialect)` 改为
   `DEFERRABLE INITIALLY DEFERRED`——同次保存内互换值不再瞬时冲突，删除了 pos 占位 hack。
4. **变体顺序**：`word_grammar_structure_variants` 增加 `sort_order`，save==read 往返对
   变体也成立（原按 dialect 排序回读）。
5. **GetTree 一致快照**：聚合读改在 REPEATABLE READ 只读事务内，杜绝与并发保存交错的撕裂树。
6. **错误映射**：23503（关联目标被并发删除）→ 400 目标不存在；未识别约束冲突不再把约束名
   泄漏进 400 body（归为 500）；关系批次前置 up.err 检查，防止 aborted-tx 把 400 掩盖成 500。
7. **富文本**：省略的 spans/liaisons 归一化为 `[]`（存储与回读均不为 null）；偏移**按 Unicode
   码点**计（JS 端必须从 UTF-16 索引换算，见 openapi RichText 描述）；上限 text 5000 码点、
   每类标注 500 条。
8. **规模上限**：单树 ≤5000 节点、短字段 ≤200 码点、保存 body ≤4 MiB。
9. **SET NULL 外键补索引**（3 个）；音频保留改为**共享契约用例**（contractEnv.setAudio 缝），
   四类载体（发音/变体/释义/例句）双实现同断言。

---

## 10. 自测清单（落地时逐项过）

- [ ] 区分方言只勾英式 → 仅英式词形即可发布；勾双方言缺美式 → V4 拒绝 `[3]`
- [ ] 不区分方言 → 全部 dialect=common；保存体混入 uk 行 → 400 `[21]`
- [ ] 原形每方言恰一条：0 条或 2 条 → V4 拒绝；同 pos 两个过去式 → 合法 `[6]`
- [ ] 草稿可只有壳；发布触发 V1–V10 全量校验并逐条报 `details`
- [ ] 保存体引用他人词条的 sense id 冒充自己节点 → 400（id 归属）
- [ ] 两管理员并发保存 → 后到者 409（乐观锁）
- [ ] 一条语法结构组被多条释义引用；删组后释义 `grammar_structure_id` 置空不报错 `[1]`
- [ ] 词条 A 关联词引用词条 B 的词义；B 保存改释义 → A 引用不断（sense id 稳定）；删 B → A 快照仍展示
- [ ] 已发布词条保存改残（删到 0 词义）→ 422（D17）
- [ ] 列表：难度列 = 词义 level 聚合；释义列 = 第一词义第一条释义；筛选/分页/统计口径正确
- [ ] 富文本 spans 越界 / liaison 越界 → 400
