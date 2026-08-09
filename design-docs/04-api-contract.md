# API 契约（OpenAPI / RESTful）

> 文档版本：v1.0 ｜ 产出人：Mora 知识库架构师 ｜ 对应任务：YS-5
> 覆盖域：文档 / 目录 / RBAC / 版本 / 检索 / RAG / MCP

---

## 1. 通用约定

### 1.1 基础信息

```yaml
openapi: 3.0.3
info:
  title: Mora 知识库平台 API
  version: 1.0.0
  description: 团队智能 Mora 与向量知识库平台 RESTful API
servers:
  - url: https://{host}/api/v1
    variables:
      host: { default: localhost:8080 }
```

### 1.2 认证

```yaml
components:
  securitySchemes:
    BearerAuth:
      type: http
      scheme: bearer
      bearerFormat: JWT          # 用户 Session JWT
    ApiKeyAuth:
      type: apiKey
      in: header
      name: Authorization         # "Bearer <api_token>" 或 "ApiKey <token>"
```

### 1.3 通用响应格式

```json
// 成功
{ "code": 0, "data": { ... }, "message": "ok" }

// 失败
{ "code": 40300, "data": null, "message": "forbidden: no permission" }
```

### 1.4 错误码

| HTTP | code | 含义 |
|---|---|---|
| 400 | 40000 | 请求参数错误 |
| 401 | 40100 | 未认证/Token 失效 |
| 403 | 40300 | 无权限（RBAC 拒绝） |
| 404 | 40400 | 资源不存在 |
| 409 | 40900 | 冲突（如标题重复） |
| 429 | 42900 | 限流 |
| 500 | 50000 | 服务器内部错误 |

### 1.5 分页

```
GET /api/v1/documents?page=1&page_size=20
→ { "code":0, "data": { "items": [...], "total": 100, "page": 1, "page_size": 20 } }
```

### 1.6 幂等与版本控制

- 写操作支持 `Idempotency-Key` 头（UUID），防重复提交。
- 文档更新支持 `If-Match` 头（ETag / version_no），乐观并发控制。

---

## 2. 认证域

### 2.1 登录

```yaml
paths:
  /auth/login:
    post:
      tags: [Auth]
      summary: 用户登录
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [email, password]
              properties:
                email: { type: string, format: email }
                password: { type: string }
      responses:
        '200':
          description: 登录成功
          content:
            application/json:
              schema:
                type: object
                properties:
                  code: { type: integer, example: 0 }
                  data:
                    type: object
                    properties:
                      token: { type: string, description: JWT }
                      user:
                        $ref: '#/components/schemas/User'
```

**示例**：

```bash
POST /api/v1/auth/login
{ "email": "alice@example.com", "password": "***" }

# 响应
{
  "code": 0,
  "data": {
    "token": "eyJhbGciOi...",
    "user": { "id": "uuid", "email": "alice@example.com", "name": "Alice" }
  }
}
```

### 2.2 API Token 管理

```yaml
  /auth/tokens:
    post:
      tags: [Auth]
      summary: 创建 API Token
      security: [BearerAuth: []]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [name, identity_type, identity_id, scope]
              properties:
                name: { type: string }
                identity_type: { type: string, enum: [user, service_account] }
                identity_id: { type: string, format: uuid }
                scope: { type: string, enum: [readonly, readwrite, admin] }
                expires_at: { type: string, format: date-time }
      responses:
        '201':
          description: 创建成功（明文 Token 仅返回一次）
          content:
            application/json:
              schema:
                type: object
                properties:
                  code: { type: integer, example: 0 }
                  data:
                    type: object
                    properties:
                      id: { type: string, format: uuid }
                      token: { type: string, description: "明文 Token，仅此一次" }
                      prefix: { type: string, example: "mora_a1b2" }
                      scope: { type: string }

    get:
      tags: [Auth]
      summary: 列出 API Token
      security: [BearerAuth: []]
      parameters:
        - { name: identity_id, in: query, schema: { type: string, format: uuid } }
      responses:
        '200':
          description: Token 列表（不含明文）

  /auth/tokens/{id}/revoke:
    post:
      tags: [Auth]
      summary: 吊销 Token
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      responses:
        '200': { description: 吊销成功 }
```

---

## 3. 工作区域

```yaml
  /workspaces:
    get:
      tags: [Workspace]
      summary: 列出当前用户可见的工作区
      security: [BearerAuth: []]
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  code: { type: integer, example: 0 }
                  data:
                    type: object
                    properties:
                      items:
                        type: array
                        items: { $ref: '#/components/schemas/Workspace' }

    post:
      tags: [Workspace]
      summary: 创建工作区
      security: [BearerAuth: []]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [name, slug]
              properties:
                name: { type: string }
                slug: { type: string }
                description: { type: string }
      responses:
        '201':
          description: 创建成功

  /workspaces/{id}:
    get:
      tags: [Workspace]
      summary: 获取工作区详情
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      responses:
        '200': { description: 工作区详情 }
```

**示例**：

```bash
GET /api/v1/workspaces
Authorization: Bearer <jwt>

# 响应
{
  "code": 0,
  "data": {
    "items": [
      { "id": "ws-uuid-1", "name": "工程团队", "slug": "eng", "owner_id": "user-uuid" }
    ]
  }
}
```

---

## 3.5 用户与角色域

> 补充查询端点（YS-13 联调发现）：供 RBAC UI 用户名/角色名显示、文档版本作者名、
> 全文检索「按创建人筛选」下拉渲染。无写操作。

```yaml
  /users:
    get:
      tags: [User]
      summary: 列出当前用户可见范围内的用户
      description: |
        RBAC 受约束：非管理员仅返回与当前用户共享至少一个可读工作区的用户
        （工作区 owner 或具 read 允许授权，含用户组继承），外加用户自身；
        管理员返回全部 active 用户。避免越权用户枚举。password_hash 永不返回。
      security: [BearerAuth: []]
      parameters:
        - { name: search, in: query, schema: { type: string }, description: "按 name/email 模糊筛选（可选）" }
        - { name: page, in: query, schema: { type: integer, default: 1 } }
        - { name: page_size, in: query, schema: { type: integer, default: 20, maximum: 100 } }
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      items:
                        type: array
                        items: { $ref: '#/components/schemas/User' }
                      total: { type: integer }
                      page: { type: integer }
                      page_size: { type: integer }
```

**示例**：

```bash
GET /api/v1/users?search=ali&page_size=20
Authorization: Bearer <jwt>

# 响应（仅含当前用户可见范围内的用户）
{
  "code": 0,
  "data": {
    "items": [
      { "id": "user-uuid", "email": "alice@mora.local", "name": "Alice", "avatar_url": "", "status": "active" }
    ],
    "total": 1,
    "page": 1,
    "page_size": 20
  }
}
```

```yaml
  /roles:
    get:
      tags: [Role]
      summary: 列出角色字典
      description: |
        返回角色列表（id / name / scope / permissions / is_system），对齐
        Permission.role_id。角色为相对静态字典，消费方可缓存。供 RBAC 配置
        与角色名显示，替代前端名称匹配 workaround。
      security: [BearerAuth: []]
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  code: { type: integer, example: 0 }
                  data:
                    type: object
                    properties:
                      items:
                        type: array
                        items: { $ref: '#/components/schemas/Role' }
```

**角色列表示例**：

```bash
GET /api/v1/roles
Authorization: Bearer <jwt>

# 响应
{
  "code": 0,
  "data": {
    "items": [
      { "id": "role-uuid-1", "name": "super_admin", "scope": "system", "permissions": ["read","write","admin"], "is_system": true },
      { "id": "role-uuid-2", "name": "workspace_admin", "scope": "workspace", "permissions": ["read","write","admin"], "is_system": true },
      { "id": "role-uuid-3", "name": "editor", "scope": "directory", "permissions": ["read","write"], "is_system": true },
      { "id": "role-uuid-4", "name": "viewer", "scope": "directory", "permissions": ["read"], "is_system": true }
    ]
  }
}
```

---

## 4. 目录域

```yaml
  /workspaces/{workspace_id}/directories:
    get:
      tags: [Directory]
      summary: 获取目录树
      security: [BearerAuth: []]
      parameters:
        - { name: workspace_id, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: parent_id, in: query, schema: { type: string, format: uuid }, description: 不传则返回根目录 }
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      items:
                        type: array
                        items: { $ref: '#/components/schemas/Directory' }

    post:
      tags: [Directory]
      summary: 创建目录
      security: [BearerAuth: []]
      parameters:
        - { name: workspace_id, in: path, required: true, schema: { type: string, format: uuid } }
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [name]
              properties:
                name: { type: string }
                parent_id: { type: string, format: uuid }
                sort_order: { type: integer }
      responses:
        '201': { description: 创建成功 }

  /directories/{id}:
    patch:
      tags: [Directory]
      summary: 更新目录（重命名/排序）
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      responses:
        '200': { description: 更新成功 }
    delete:
      tags: [Directory]
      summary: 删除目录（级联删除子目录与文档）
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      responses:
        '204': { description: 删除成功 }
```

---

## 5. 文档域

```yaml
  /workspaces/{workspace_id}/documents:
    get:
      tags: [Document]
      summary: 列出文档
      security: [BearerAuth: []]
      parameters:
        - { name: workspace_id, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: directory_id, in: query, schema: { type: string, format: uuid } }
        - { name: tag, in: query, schema: { type: string } }
        - { name: status, in: query, schema: { type: string, enum: [draft, published, archived] } }
        - { name: created_by, in: query, schema: { type: string, format: uuid } }
        - { name: updated_after, in: query, schema: { type: string, format: date-time } }
        - { name: page, in: query, schema: { type: integer, default: 1 } }
        - { name: page_size, in: query, schema: { type: integer, default: 20 } }
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      items:
                        type: array
                        items: { $ref: '#/components/schemas/Document' }
                      total: { type: integer }
                      page: { type: integer }
                      page_size: { type: integer }

    post:
      tags: [Document]
      summary: 创建文档
      security: [BearerAuth: []]
      parameters:
        - { name: workspace_id, in: path, required: true, schema: { type: string, format: uuid } }
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [title]
              properties:
                title: { type: string }
                directory_id: { type: string, format: uuid }
                content:
                  type: array
                  description: Block 数组
                  items: { $ref: '#/components/schemas/Block' }
                format: { type: string, enum: [blocks, markdown], default: blocks }
                tags: { type: array, items: { type: string, format: uuid } }
      responses:
        '201':
          description: 创建成功
          content:
            application/json:
              schema:
                type: object
                properties:
                  data: { $ref: '#/components/schemas/Document' }
```

**创建文档示例**：

```bash
POST /api/v1/workspaces/ws-uuid-1/documents
Authorization: Bearer <jwt>
Content-Type: application/json

{
  "title": "API 设计规范",
  "directory_id": "dir-uuid-1",
  "format": "blocks",
  "content": [
    { "type": "heading", "attrs": { "level": 1 }, "content": [{ "type": "text", "text": "API 设计规范" }] },
    { "type": "paragraph", "content": [{ "type": "text", "text": "本文档定义 RESTful API 设计规范。" }] },
    { "type": "codeBlock", "attrs": { "language": "bash" }, "content": [{ "type": "text", "text": "curl https://..." }] }
  ],
  "tags": ["tag-uuid-1"]
}

# 响应 201
{
  "code": 0,
  "data": {
    "id": "doc-uuid-1",
    "workspace_id": "ws-uuid-1",
    "directory_id": "dir-uuid-1",
    "title": "API 设计规范",
    "status": "draft",
    "index_status": "pending",
    "version_no": 1,
    "created_by": "user-uuid",
    "created_at": "2026-07-29T08:00:00Z"
  }
}
```

```yaml
  /documents/{id}:
    get:
      tags: [Document]
      summary: 获取文档详情
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  data: { $ref: '#/components/schemas/Document' }
        '403': { description: 无读权限 }
        '404': { description: 文档不存在或无权查看 }

    patch:
      tags: [Document]
      summary: 更新文档内容
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: If-Match, in: header, schema: { type: string }, description: "版本号乐观锁" }
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                title: { type: string }
                content:
                  type: array
                  items: { $ref: '#/components/schemas/Block' }
                directory_id: { type: string, format: uuid }
                status: { type: string, enum: [draft, published, archived] }
                tags: { type: array, items: { type: string, format: uuid } }
                summary: { type: string, description: "变更摘要" }
      responses:
        '200':
          description: 更新成功，产生新版本
        '409': { description: 版本冲突 }

    delete:
      tags: [Document]
      summary: 删除文档（软删除，触发级联向量清理）
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      responses:
        '204': { description: 删除成功 }
```

---

## 6. 版本历史域

```yaml
  /documents/{id}/versions:
    get:
      tags: [Version]
      summary: 列出文档版本历史
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: page, in: query, schema: { type: integer } }
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      items:
                        type: array
                        items: { $ref: '#/components/schemas/DocumentVersion' }

  /documents/{id}/versions/diff:
    get:
      tags: [Version]
      summary: 比较两个版本的 Diff
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: from, in: query, required: true, schema: { type: integer }, description: 起始版本号 }
        - { name: to, in: query, required: true, schema: { type: integer }, description: 目标版本号 }
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      diff: { type: array, description: "Block 级 diff" }

  /documents/{id}/versions/{version_no}/rollback:
    post:
      tags: [Version]
      summary: 回滚到指定版本（产生新版本）
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: version_no, in: path, required: true, schema: { type: integer } }
      responses:
        '200':
          description: 回滚成功，返回新版本号
```

**Diff 示例**：

```bash
GET /api/v1/documents/doc-uuid-1/versions/diff?from=3&to=5
Authorization: Bearer <jwt>

# 响应
{
  "code": 0,
  "data": {
    "from_version": 3,
    "to_version": 5,
    "diff": [
      { "type": "modified", "block_id": "blk-1", "from": "旧内容", "to": "新内容" },
      { "type": "added", "block_id": "blk-2", "content": "新增段落" },
      { "type": "removed", "block_id": "blk-3", "content": "删除段落" }
    ]
  }
}
```

---

## 7. RBAC 权限域

```yaml
  /permissions:
    get:
      tags: [RBAC]
      summary: 查询权限授权
      security: [BearerAuth: []]
      parameters:
        - { name: target_type, in: query, schema: { type: string, enum: [workspace, directory, document] } }
        - { name: target_id, in: query, schema: { type: string, format: uuid } }
        - { name: subject_id, in: query, schema: { type: string, format: uuid } }
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      items:
                        type: array
                        items: { $ref: '#/components/schemas/Permission' }

    post:
      tags: [RBAC]
      summary: 授权（添加权限）
      security: [BearerAuth: []]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [subject_type, subject_id, role_id, target_type, target_id]
              properties:
                subject_type: { type: string, enum: [user, group] }
                subject_id: { type: string, format: uuid }
                role_id: { type: string, format: uuid }
                target_type: { type: string, enum: [workspace, directory, document] }
                target_id: { type: string, format: uuid }
                effect: { type: string, enum: [allow, deny], default: allow }
                inherit_scope: { type: string, enum: [node_only, subtree], default: subtree }
      responses:
        '201': { description: 授权成功 }

  /permissions/{id}:
    delete:
      tags: [RBAC]
      summary: 撤销权限授权
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      responses:
        '204': { description: 撤销成功 }

  /permissions/check:
    post:
      tags: [RBAC]
      summary: 检查权限
      security: [BearerAuth: []]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [target_type, target_id, action]
              properties:
                target_type: { type: string, enum: [workspace, directory, document] }
                target_id: { type: string, format: uuid }
                action: { type: string, enum: [read, write, admin] }
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      allowed: { type: boolean }
```

---

## 8. 全文检索域

```yaml
  /search:
    get:
      tags: [Search]
      summary: 全文检索（BM25 + 多维筛选 + RBAC 过滤）
      security: [BearerAuth: []]
      parameters:
        - { name: q, in: query, required: true, schema: { type: string }, description: 搜索关键词 }
        - { name: workspace_id, in: query, schema: { type: string, format: uuid } }
        - { name: directory_id, in: query, schema: { type: string, format: uuid } }
        - { name: tag, in: query, schema: { type: string } }
        - { name: created_by, in: query, schema: { type: string, format: uuid } }
        - { name: updated_after, in: query, schema: { type: string, format: date-time } }
        - { name: updated_before, in: query, schema: { type: string, format: date-time } }
        - { name: doc_type, in: query, schema: { type: string } }
        - { name: sort, in: query, schema: { type: string, enum: [relevance, updated], default: relevance } }
        - { name: page, in: query, schema: { type: integer, default: 1 } }
        - { name: page_size, in: query, schema: { type: integer, default: 20 } }
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      items:
                        type: array
                        items:
                          type: object
                          properties:
                            document_id: { type: string, format: uuid }
                            title: { type: string }
                            snippet: { type: string, description: "命中片段，高亮" }
                            highlight: { type: array, items: { type: string } }
                            score: { type: number }
                            workspace_id: { type: string }
                            directory_id: { type: string }
                            updated_at: { type: string, format: date-time }
                      total: { type: integer }
```

**检索示例**：

```bash
GET /api/v1/search?q=API设计规范&workspace_id=ws-uuid-1&sort=relevance&page_size=10
Authorization: Bearer <jwt>

# 响应（仅返回当前用户有权读的文档，无权文档不出现）
{
  "code": 0,
  "data": {
    "items": [
      {
        "document_id": "doc-uuid-1",
        "title": "API 设计规范",
        "snippet": "本文档定义 <em>RESTful API</em> 设计规范...",
        "highlight": ["RESTful API"],
        "score": 0.95,
        "workspace_id": "ws-uuid-1",
        "directory_id": "dir-uuid-1",
        "updated_at": "2026-07-29T08:00:00Z"
      }
    ],
    "total": 1,
    "page": 1,
    "page_size": 10
  }
}
```

---

## 9. RAG 语义检索域

```yaml
  /rag/search:
    post:
      tags: [RAG]
      summary: 语义混合检索（Dense + BM25 + RBAC payload 过滤）
      security: [BearerAuth: []]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [query]
              properties:
                query: { type: string, description: 自然语言查询 }
                workspace_id: { type: string, format: uuid }
                directory_id: { type: string, format: uuid }
                tags: { type: array, items: { type: string } }
                top_k: { type: integer, default: 50, description: 各路召回数 }
                top_n: { type: integer, default: 10, description: 最终返回数 }
                rerank: { type: boolean, default: false, description: 是否启用 Reranker（P1） }
                filters:
                  type: object
                  description: 额外元数据过滤
                  properties:
                    updated_after: { type: string, format: date-time }
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      items:
                        type: array
                        items:
                          type: object
                          properties:
                            document_id: { type: string, format: uuid }
                            title: { type: string }
                            chunk_text: { type: string, description: 命中 chunk 片段 }
                            chunk_index: { type: integer }
                            score: { type: number, description: 融合/重排得分 }
                            dense_score: { type: number }
                            bm25_score: { type: number }
                            workspace_id: { type: string }
                            source_url: { type: string, description: "文档跳转链接" }
                      total: { type: integer }
```

**语义检索示例**：

```bash
POST /api/v1/rag/search
Authorization: Bearer <jwt>
Content-Type: application/json

{
  "query": "如何设计 RESTful API 的分页",
  "workspace_id": "ws-uuid-1",
  "top_k": 50,
  "top_n": 10,
  "rerank": true
}

# 响应（结果经 RBAC payload 过滤，无权文档不返回）
{
  "code": 0,
  "data": {
    "items": [
      {
        "document_id": "doc-uuid-1",
        "title": "API 设计规范",
        "chunk_text": "分页采用 page/page_size 参数，响应包含 total/page/page_size...",
        "chunk_index": 3,
        "score": 0.92,
        "dense_score": 0.88,
        "bm25_score": 0.65,
        "workspace_id": "ws-uuid-1",
        "source_url": "/workspaces/ws-uuid-1/documents/doc-uuid-1"
      }
    ],
    "total": 10
  }
}
```

### 9.1 RAG 索引状态查询

```yaml
  /documents/{id}/index-status:
    get:
      tags: [RAG]
      summary: 查询文档知识索引状态
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      index_status: { type: string, enum: [pending, processing, indexed, failed] }
                      last_indexed_at: { type: string, format: date-time }
                      chunk_count: { type: integer }
                      error: { type: string }
```

### 9.2 Embedding 模型配置

```yaml
  /admin/embedding-models:
    get:
      tags: [RAG Admin]
      summary: 列出 Embedding 模型配置
      security: [BearerAuth: []]
      responses:
        '200': { description: 模型列表 }

    post:
      tags: [RAG Admin]
      summary: 添加/更新 Embedding 模型配置
      security: [BearerAuth: []]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [provider, model_name, dimension]
              properties:
                provider: { type: string, enum: [tei, ollama] }
                model_name: { type: string }
                dimension: { type: integer }
                max_token: { type: integer }
                instruction_query: { type: string }
                instruction_doc: { type: string }
      responses:
        '201': { description: 配置成功 }

  /admin/embedding-models/{id}/test:
    post:
      tags: [RAG Admin]
      summary: 连通性测试
      security: [BearerAuth: []]
      responses:
        '200':
          description: 测试结果
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      ok: { type: boolean }
                      latency_ms: { type: integer }
                      dimension: { type: integer }

  /admin/embedding-models/{id}/rebuild:
    post:
      tags: [RAG Admin]
      summary: 触发存量向量重建
      security: [BearerAuth: []]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                workspace_id: { type: string, format: uuid, description: 可选，仅重建指定工作区 }
      responses:
        '202': { description: 重建任务已提交 }
```

---

## 10. MCP 域（REST 辅助接口）

> MCP 协议端点（HTTP/SSE）独立于 REST API，部署在 `mcp-server:8081`。
> 以下为 MCP 协议端点的 REST 辅助管理与健康检查接口。

```yaml
  /mcp/health:
    get:
      tags: [MCP]
      summary: MCP Server 健康检查
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  status: { type: string, example: "ok" }
                  version: { type: string, example: "1.0.0" }

  /mcp/sessions:
    get:
      tags: [MCP Admin]
      summary: 列出 MCP 会话（管理用）
      security: [BearerAuth: []]
      responses:
        '200': { description: 会话列表 }

  /mcp/tool-calls:
    get:
      tags: [MCP Admin]
      summary: 查询工具调用记录（审计）
      security: [BearerAuth: []]
      parameters:
        - { name: token_id, in: query, schema: { type: string, format: uuid } }
        - { name: tool_name, in: query, schema: { type: string } }
        - { name: since, in: query, schema: { type: string, format: date-time } }
      responses:
        '200': { description: 调用记录列表 }
```

### 10.1 MCP 协议端点（非 REST，JSON-RPC over HTTP/SSE）

MCP 协议遵循 MCP 规范，使用 JSON-RPC 2.0 over HTTP/SSE：

```yaml
  /mcp/sse:
    get:
      tags: [MCP Protocol]
      summary: MCP SSE 传输端点（JSON-RPC over SSE）
      description: |
        Agent 通过 SSE 连接此端点，发送 JSON-RPC 请求。
        遵循 MCP 规范：initialize → capabilities → tools/list → tools/call。
      security: [ApiKeyAuth: []]
      responses:
        '200':
          description: SSE 流
          content:
            text/event-stream:
              schema:
                type: string

  /mcp/messages:
    post:
      tags: [MCP Protocol]
      summary: MCP 消息端点（JSON-RPC over HTTP POST）
      description: |
        Agent 通过 HTTP POST 发送 JSON-RPC 请求。
        支持 initialize, tools/list, tools/call, resources/list, resources/read。
      security: [ApiKeyAuth: []]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              description: JSON-RPC 2.0 请求
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                description: JSON-RPC 2.0 响应
```

---

## 11. 附件域

```yaml
  /documents/{id}/attachments:
    post:
      tags: [Attachment]
      summary: 上传附件
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      requestBody:
        content:
          multipart/form-data:
            schema:
              type: object
              properties:
                file:
                  type: string
                  format: binary
      responses:
        '201':
          content:
            application/json:
              schema:
                type: object
                properties:
                  data: { $ref: '#/components/schemas/Attachment' }

  /attachments/{id}/download:
    get:
      tags: [Attachment]
      summary: 下载附件（鉴权后重定向预签名 URL 或代理下载）
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      responses:
        '302': { description: 重定向到预签名 URL }
        '200': { description: 代理下载流 }
```

---

## 12. 导入导出域

```yaml
  /workspaces/{workspace_id}/import:
    post:
      tags: [Import/Export]
      summary: 批量导入文档（Markdown/PDF/Docx/HTML/ZIP）
      security: [BearerAuth: []]
      parameters:
        - { name: workspace_id, in: path, required: true, schema: { type: string, format: uuid } }
      requestBody:
        content:
          multipart/form-data:
            schema:
              type: object
              properties:
                file: { type: string, format: binary }
                directory_id: { type: string, format: uuid }
                conflict_strategy: { type: string, enum: [overwrite, skip, append], default: append }
      responses:
        '202':
          description: 导入任务已提交（异步）
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      task_id: { type: string, format: uuid }
                      status: { type: string, example: "processing" }

  /documents/{id}/export:
    get:
      tags: [Import/Export]
      summary: 导出文档
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: format, in: query, required: true, schema: { type: string, enum: [markdown, pdf, html] } }
      responses:
        '200': { description: 文件流 }
        '302': { description: 重定向到预签名 URL }
```

---

## 13. 评论域

```yaml
  /documents/{id}/comments:
    get:
      tags: [Comment]
      summary: 列出文档评论
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
        - { name: block_id, in: query, schema: { type: string, format: uuid }, description: 按块筛选 }
      responses:
        '200': { description: 评论列表 }

    post:
      tags: [Comment]
      summary: 添加评论
      security: [BearerAuth: []]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [content]
              properties:
                content: { type: string }
                block_id: { type: string, format: uuid }
                parent_id: { type: string, format: uuid, description: 回复某评论 }
                mentions: { type: array, items: { type: string, format: uuid } }
      responses:
        '201': { description: 评论创建成功 }

  /comments/{id}/resolve:
    post:
      tags: [Comment]
      summary: 标记评论已解决
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      responses:
        '200': { description: 标记成功 }
```

---

## 14. 协同编辑域（WebSocket）

```
WebSocket: /api/v1/ws/collab/{document_id}
```

- 协议：Yjs sync protocol over WebSocket
- 鉴权：连接时携带 `?token=<jwt>`，校验文档写权限
- yjs-server 转发 Yjs update 消息，维护 awareness（光标/在线状态）
- 多副本：粘性路由（同一文档路由到同一 yjs-server 实例）

---

## 15. Schema 定义汇总

```yaml
components:
  schemas:
    User:
      type: object
      properties:
        id: { type: string, format: uuid }
        email: { type: string }
        name: { type: string }
        avatar_url: { type: string }
        status: { type: string }

    Role:
      type: object
      description: 角色字典项，Permission.role_id 引用其 id
      properties:
        id: { type: string, format: uuid }
        name: { type: string, example: viewer }
        scope: { type: string, enum: [system, workspace, directory, page] }
        workspace_id: { type: string, format: uuid, description: "workspace 以下级角色关联的工作区（系统级为空）" }
        permissions:
          type: array
          items: { type: string, enum: [read, write, admin] }
        is_system: { type: boolean, description: 系统内置角色 }
        created_at: { type: string, format: date-time }

    Workspace:
      type: object
      properties:
        id: { type: string, format: uuid }
        name: { type: string }
        slug: { type: string }
        description: { type: string }
        owner_id: { type: string, format: uuid }
        settings: { type: object }
        created_at: { type: string, format: date-time }

    Directory:
      type: object
      properties:
        id: { type: string, format: uuid }
        workspace_id: { type: string, format: uuid }
        parent_id: { type: string, format: uuid }
        name: { type: string }
        path: { type: string }
        sort_order: { type: integer }
        children: { type: array, items: { $ref: '#/components/schemas/Directory' } }

    Document:
      type: object
      properties:
        id: { type: string, format: uuid }
        workspace_id: { type: string, format: uuid }
        directory_id: { type: string, format: uuid }
        title: { type: string }
        content:
          type: array
          items: { $ref: '#/components/schemas/Block' }
        format: { type: string }
        status: { type: string }
        index_status: { type: string }
        version_no: { type: integer }
        tags: { type: array, items: { type: string } }
        created_by: { type: string, format: uuid }
        updated_by: { type: string, format: uuid }
        created_at: { type: string, format: date-time }
        updated_at: { type: string, format: date-time }

    Block:
      type: object
      properties:
        type: { type: string, description: "text/heading/codeBlock/chart/canvas" }
        attrs: { type: object }
        content:
          type: array
          items: { type: object }

    DocumentVersion:
      type: object
      properties:
        id: { type: string, format: uuid }
        document_id: { type: string, format: uuid }
        version_no: { type: integer }
        diff_summary: { type: string }
        author_id: { type: string, format: uuid }
        created_at: { type: string, format: date-time }

    Permission:
      type: object
      properties:
        id: { type: string, format: uuid }
        subject_type: { type: string }
        subject_id: { type: string, format: uuid }
        role_id: { type: string, format: uuid }
        target_type: { type: string }
        target_id: { type: string, format: uuid }
        effect: { type: string }
        inherit_scope: { type: string }

    Attachment:
      type: object
      properties:
        id: { type: string, format: uuid }
        document_id: { type: string, format: uuid }
        name: { type: string }
        mime_type: { type: string }
        size_bytes: { type: integer }
        created_at: { type: string, format: date-time }
```

---

## 16. API 速率限制

| 接口域 | 用户级限流 | Token 级限流（MCP） |
|---|---|---|
| 文档读写 | 300 req/min | — |
| 检索（FTS + RAG） | 200 req/min | 100 req/min |
| MCP tools/call | — | 100 req/min（可配） |
| 附件上传 | 60 req/min | — |
| 导入导出 | 10 req/min | — |

超限返回 `429` + `Retry-After` 头。

---

> 本 API 契约为 Stage 1 门禁交付物，覆盖 PRD 全部功能域。Stage 2 前后端可据此并行开发（mock 契约先行）。
