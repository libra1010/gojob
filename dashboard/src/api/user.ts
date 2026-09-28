import http, { request } from './http'
import type { LoginReq, LoginResp, MeResp, UserInfo, UserReq } from './types'

export function login(req: LoginReq) {
  return request<LoginResp>(http.post('/user/login', req))
}

export function logout() {
  return request<Record<string, never>>(http.post('/user/logout'))
}

export function getMe() {
  return request<MeResp>(http.get('/user/me'))
}

/** 修改自己的密码（校验旧密码，成功后旧 token 失效） */
export function changePassword(oldPassword: string, newPassword: string) {
  return request<Record<string, never>>(
    http.post('/user/changePassword', { old_password: oldPassword, new_password: newPassword }),
  )
}

// ---------- 账号管理（仅 SUPER_ADMIN） ----------

export function listUsers() {
  return request<UserInfo[]>(http.get('/user/list'))
}

export function createUser(req: UserReq) {
  return request<number>(http.post('/user/create', req))
}

export function modifyUser(req: UserReq) {
  return request<Record<string, never>>(http.post('/user/modify', req))
}

export function resetPassword(username: string, newPassword: string) {
  return request<Record<string, never>>(http.post('/user/resetPassword', { username, new_password: newPassword }))
}

export function setUserStatus(username: string, status: 'ACTIVE' | 'DISABLED') {
  return request<Record<string, never>>(http.post('/user/status', { username, status }))
}

export function deleteUser(username: string) {
  return request<Record<string, never>>(http.post('/user/delete', { username }))
}
