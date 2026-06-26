# 前端 Token 存储指南（access token + refresh token）

> 给前端同学看的。读完你应该清楚:**每个 token 放哪、怎么发、为什么这么定**,以及从旧版(token 存 cookie / localStorage)迁移要改哪几行。

## TL;DR

| Token | 存哪 | 谁来管 | 怎么发给后端 |
|---|---|---|---|
| **access token**(短期,15 分钟) | **内存**(JS 变量 / 状态管理) | 前端 | 请求头 `Authorization: Bearer <access_token>` |
| **refresh token**(长期,30 天) | **HttpOnly cookie** | 后端下发,前端**碰不到** | 浏览器自动随请求带上,前端不用管 |

一句话:**access token 你拿在手里走 header;refresh token 你看不见也不用存,后端发一个 HttpOnly cookie,浏览器自己带。**

---

## 为什么这么定

之前 token 存在**非 HttpOnly 的 cookie**(或 localStorage)里,这其实是最不安全的一档。对比一下:

| 存储方式 | XSS 能否偷到 token | 是否自动随请求发送(CSRF 面) |
|---|---|---|
| 内存(JS 变量) | 能,但刷新页面即失效 | 否 |
| localStorage | 能,且长期留存 | 否 |
| **非 HttpOnly cookie**(旧方案) | **能** | **会** |
| **HttpOnly cookie**(新方案) | **不能**(JS 读不到) | 会 → 用 SameSite 防住 |

非 HttpOnly cookie 同时吃了 **XSS** 和 **CSRF** 两个坑。新方案把两类 token 分开放,各取其长:

- **access token 放内存**:就算被 XSS,偷到的也是一个 15 分钟就过期的短命令牌,刷新页面还会丢,损失有限。
- **refresh token 放 HttpOnly cookie**:这是能换 30 天访问权的"主钥匙",所以必须让 JS 完全读不到(`HttpOnly`),只走 HTTPS(`Secure`),且不跟随跨站请求(`SameSite=Strict`)。XSS 偷不走,CSRF 也被挡住。

---

## 后端的 Cookie 是怎么下发的

`register` / `login` / `login/code` / `refresh` 成功时,后端会在响应里带一个 `Set-Cookie`:

```
Set-Cookie: refresh_token=<token>; Path=/api/v1/auth; Max-Age=2592000; HttpOnly; Secure; SameSite=Strict
```

各属性的含义:

| 属性 | 作用 |
|---|---|
| `HttpOnly` | JS 读不到(`document.cookie` 看不见它),XSS 偷不走 —— 核心防护 |
| `Secure` | 只在 HTTPS 下发送(本地 http 开发环境后端会自动关掉,见下) |
| `SameSite=Strict` | 跨站请求不携带,挡住 CSRF |
| `Path=/api/v1/auth` | 只有 `/api/v1/auth/*` 下的接口会带上它,普通业务接口收不到,暴露面最小 |
| `Max-Age` | cookie 寿命,与 refresh token 的 TTL 一致(默认 30 天) |

> **本地开发提示**:后端在 `APP_ENV=development` 时会把 `Secure` 关掉,否则浏览器会拒绝在 http 下保存 cookie。生产环境务必走 HTTPS。

---

## 前端要做什么(很少)

### 1. 请求带上 cookie:`withCredentials`

cookie 不会自动跨实例携带,axios/fetch 必须显式开启凭证:

```ts
// axios
const http = axios.create({ baseURL: '/api/v1', withCredentials: true })

// 或 fetch
fetch('/api/v1/auth/refresh', { method: 'POST', credentials: 'include' })
```

### 2. access token 放内存,登录后保存

```ts
// 登录成功:body 里只有 access_token,refresh token 不在 body 里(它在 cookie 里)
const { data } = await http.post('/auth/login', { identifier, password })
setAccessToken(data.access_token)   // 存到内存 / 状态管理,不要写 localStorage
```

### 3. 每个请求带上 access token

```ts
http.interceptors.request.use(config => {
  const token = getAccessToken()              // 从内存读
  if (token) config.headers.Authorization = `Bearer ${token}`
  return config
})
```

### 4. 刷新:不传 refresh token,后端从 cookie 读

```ts
// 注意:body 是空的!refresh token 由浏览器通过 cookie 自动带上
const { data } = await http.post('/auth/refresh')
setAccessToken(data.access_token)             // 响应 body 只回新的 access_token
// 新的 refresh token 由后端通过 Set-Cookie 自动轮换,前端无需处理
```

### 5. 登出:也不用传 refresh token

```ts
await http.post('/auth/logout')               // 后端从 cookie 读并撤销,同时清掉 cookie
clearAccessToken()                            // 前端清掉内存里的 access token
```

---

## 完整示例(axios 拦截器)

```ts
// http.ts
import axios from 'axios'

let accessToken: string | null = null
export const setAccessToken = (t: string | null) => { accessToken = t }
export const getAccessToken = () => accessToken

const http = axios.create({ baseURL: '/api/v1', withCredentials: true })

// 每个请求带上内存里的 access token
http.interceptors.request.use(config => {
  if (accessToken) config.headers.Authorization = `Bearer ${accessToken}`
  return config
})

// 401 统一处理
http.interceptors.response.use(
  res => res,
  async err => {
    if (!axios.isAxiosError(err)) return Promise.reject(err)
    const status = err.response?.status
    const errorMsg = err.response?.data?.error
    const url = err.config?.url ?? ''

    // ① 登录类接口的 401:凭证错误,交给调用方
    if (status === 401 && (url.includes('/auth/login') || url.includes('/auth/login/code'))) {
      return Promise.reject(err)
    }

    // ② refresh 自己 401:会话彻底结束
    if (status === 401 && url.includes('/auth/refresh')) {
      setAccessToken(null)
      window.location.href = '/login'
      return Promise.reject(err)
    }

    // ③ 业务接口 access token 过期:刷新一次再重试
    if (status === 401 && errorMsg === 'invalid or expired token' && !err.config._retry) {
      err.config._retry = true
      try {
        const { data } = await http.post('/auth/refresh')   // 不传 body,cookie 自动带
        setAccessToken(data.access_token)
        err.config.headers.Authorization = `Bearer ${data.access_token}`
        return http(err.config)                             // 重放原请求
      } catch {
        return Promise.reject(err)                          // refresh 失败 → 走 ②
      }
    }

    return Promise.reject(err)
  }
)

export default http
```

```ts
// api/auth.ts
import http, { setAccessToken } from './http'

export async function login(identifier: string, password: string) {
  try {
    const { data } = await http.post('/auth/login', { identifier, password })
    setAccessToken(data.access_token)   // refresh token 在 cookie 里,无需保存
    return data
  } catch (err) {
    if (axios.isAxiosError(err) && err.response?.status === 401) {
      throw new Error('手机号或密码错误')
    }
    throw err
  }
}
```

---

## "刷新页面就掉登录"怎么办?

access token 放内存,刷新页面确实会丢。**这是预期行为,不是 bug**,靠 refresh cookie 恢复即可:

应用启动时(或首屏)先静默调一次 `/auth/refresh`:

```ts
// 应用入口
try {
  const { data } = await http.post('/auth/refresh')  // cookie 还在 → 拿到新 access token
  setAccessToken(data.access_token)
  // 已登录,进主页面
} catch {
  // cookie 没了或失效 → 未登录,去登录页
}
```

只要 refresh cookie 没过期(30 天)且没被登出/挤下线,刷新页面后这步就能无感恢复登录态。

---

## 部署注意:跨域 / 跨站

cookie 的"同站(SameSite)"判定基于**可注册域名(eTLD+1)**,不是端口或子域:

- **同域 / 同站**(前后端在同一域名,或 `app.example.com` 调 `api.example.com`)→ `SameSite=Strict` 正常工作,cookie 会带上。**推荐这样部署**(前端用反向代理或同主域)。
- **完全跨站**(前端 `foo.com` 调后端 `bar.com`)→ `SameSite=Strict/Lax` 的 cookie 在跨站请求里**根本不会发送**,refresh 会失败。这种情况需要后端改成 `SameSite=None; Secure` 并配置带凭证的 CORS(`Access-Control-Allow-Credentials: true` + 指定 `Allow-Origin`),同时**必须额外加 CSRF token 防护**。

如果你们是完全跨站部署,提前跟后端说一声,这块要专门配。

---

## 迁移清单(从旧方案改过来)

- [ ] 请求实例加 `withCredentials: true` / `credentials: 'include'`
- [ ] 删掉所有 `localStorage.setItem('refresh_token', ...)` / `getItem('refresh_token')` / `removeItem('refresh_token')`
- [ ] access token 从 localStorage 改为内存(状态管理)
- [ ] `/auth/refresh` 调用去掉 `{ refresh_token }` 请求体
- [ ] `/auth/logout` 调用去掉 `{ refresh_token }` 请求体
- [ ] 登录/注册响应不再读 `data.refresh_token`(后端已不返回)
- [ ] 应用启动时静默调用一次 `/auth/refresh` 恢复登录态

接口字段的权威定义见 [api.md](api.md)。
