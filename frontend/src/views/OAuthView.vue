<template>
  <div>
    <div class="page-hd">
      <div>
        <h1>OAuth 连接器</h1>
        <p class="page-sub">为 Claude 等远程 MCP 客户端签发 OAuth 2.1 客户端凭据。</p>
      </div>
      <div class="page-actions">
        <el-button type="primary" :icon="Plus" circle title="新增 OAuth 客户端" @click="openCreate" />
      </div>
    </div>

    <el-alert v-if="createdClient" type="success" show-icon :closable="false" class="secret-alert">
      <template #title>
        <div class="secret-title">客户端密钥只显示一次</div>
      </template>
      <div class="secret-grid">
        <div><small>Client ID</small><code>{{ createdClient.client_id }}</code><el-button :icon="CopyDocument" circle size="small" title="复制 Client ID" @click="copyText(createdClient.client_id)" /></div>
        <div><small>Client Secret</small><code>{{ createdClient.client_secret }}</code><el-button :icon="CopyDocument" circle size="small" title="复制 Client Secret" @click="copyText(createdClient.client_secret || '')" /></div>
      </div>
    </el-alert>

    <section class="endpoint-band">
      <div><strong>Remote MCP server URL</strong><code>{{ origin }}/mcp</code><el-button :icon="CopyDocument" circle size="small" title="复制 MCP 地址" @click="copyText(origin + '/mcp')" /></div>
      <div><strong>OAuth 授权端点</strong><code>{{ origin }}/oauth/authorize</code></div>
      <div><strong>Token 端点</strong><code>{{ origin }}/oauth/token</code></div>
    </section>

    <el-card class="soft-card" shadow="never" v-loading="loading">
      <el-table :data="clients" stripe>
        <el-table-column prop="name" label="客户端" min-width="150" />
        <el-table-column prop="client_id" label="Client ID" min-width="180">
          <template #default="scope"><span class="mono">{{ scope.row.client_id }}</span></template>
        </el-table-column>
        <el-table-column label="回调地址" min-width="240">
          <template #default="scope"><div class="uri-list"><code v-for="uri in scope.row.redirect_uris" :key="uri">{{ uri }}</code></div></template>
        </el-table-column>
        <el-table-column label="权限渠道" min-width="130">
          <template #default="scope"><div class="provider-tags"><el-tag v-if="scope.row.allowed_providers.length === 0" size="small" type="info">全部</el-tag><el-tag v-for="provider in scope.row.allowed_providers" :key="provider" size="small">{{ providerLabel(provider) }}</el-tag></div></template>
        </el-table-column>
        <el-table-column label="状态" width="90"><template #default="scope"><el-tag :type="scope.row.status === 'enabled' ? 'success' : 'info'">{{ scope.row.status === 'enabled' ? '启用' : '停用' }}</el-tag></template></el-table-column>
        <el-table-column label="" width="132" align="right">
          <template #default="scope"><div class="row-actions"><el-button link :icon="Edit" title="编辑" @click="openEdit(scope.row)" /><el-button link :icon="scope.row.status === 'enabled' ? Remove : CircleCheck" :title="scope.row.status === 'enabled' ? '停用' : '启用'" @click="setStatus(scope.row, scope.row.status === 'enabled' ? 'disabled' : 'enabled')" /><el-button link type="danger" :icon="Delete" title="删除" @click="remove(scope.row.id)" /></div></template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-dialog v-model="dialog" :title="editingClient ? '编辑 OAuth 客户端' : '新增 OAuth 客户端'" width="560px">
      <el-form label-position="top">
        <el-form-item label="名称"><el-input v-model="form.name" placeholder="例如 Claude 手机端" /></el-form-item>
        <el-form-item label="允许的回调地址"><el-input v-model="redirectURIsText" type="textarea" :rows="3" placeholder="每行一个 HTTPS 回调地址" /></el-form-item>
        <el-form-item label="允许请求渠道"><el-select v-model="form.allowed_providers" multiple collapse-tags collapse-tags-tooltip placeholder="不选择表示全部渠道"><el-option v-for="item in providerOptions" :key="item.value" :label="item.label" :value="item.value" /></el-select></el-form-item>
      </el-form>
      <template #footer><el-button @click="dialog = false">取消</el-button><el-button type="primary" @click="save">保存</el-button></template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus/es/components/message/index'
import { CircleCheck, CopyDocument, Delete, Edit, Plus, Remove } from '@element-plus/icons-vue'
import { api, OAuthClient } from '../api/client'
import { providerLabel, providerOptions } from '../utils/providers'

const loading = ref(true)
const clients = ref<OAuthClient[]>([])
const dialog = ref(false)
const editingClient = ref<OAuthClient | null>(null)
const createdClient = ref<OAuthClient | null>(null)
const redirectURIsText = ref('https://claude.ai/oauth/callback')
const origin = window.location.origin
const form = reactive({ name: 'Claude 连接器', allowed_providers: ['brave', 'tavily', 'exa', 'grok'] as string[] })

async function load() { loading.value = true; try { clients.value = (await api.oauthClients()).clients } finally { loading.value = false } }
function reset() { form.name = 'Claude 连接器'; form.allowed_providers = ['brave', 'tavily', 'exa', 'grok']; redirectURIsText.value = 'https://claude.ai/oauth/callback' }
function openCreate() { editingClient.value = null; reset(); dialog.value = true }
function openEdit(client: OAuthClient) { editingClient.value = client; form.name = client.name; form.allowed_providers = [...client.allowed_providers]; redirectURIsText.value = client.redirect_uris.join('\n'); dialog.value = true }
function redirectURIs() { return redirectURIsText.value.split('\n').map(value => value.trim()).filter(Boolean) }
async function copyText(value: string) { await navigator.clipboard.writeText(value); ElMessage.success('已复制') }
async function save() {
  const payload = { name: form.name.trim(), redirect_uris: redirectURIs(), allowed_providers: form.allowed_providers }
  if (!payload.name || payload.redirect_uris.length === 0) { ElMessage.error('请填写名称和至少一个回调地址'); return }
  if (editingClient.value) { await api.updateOAuthClient(editingClient.value.id, { ...payload, status: editingClient.value.status }); ElMessage.success('OAuth 客户端已保存') }
  else { createdClient.value = await api.createOAuthClient(payload); ElMessage.success('OAuth 客户端已创建') }
  dialog.value = false; await load()
}
async function setStatus(client: OAuthClient, status: string) { await api.updateOAuthClient(client.id, { name: client.name, redirect_uris: client.redirect_uris, allowed_providers: client.allowed_providers, status }); await load() }
async function remove(id: number) { await api.deleteOAuthClient(id); await load() }
onMounted(load)
</script>

<style scoped>
.page-sub { margin: 5px 0 0; color: var(--faint); font-size: 13px; }
.secret-alert { margin-bottom: 14px; }
.secret-title { font-weight: 700; }
.secret-grid { display: grid; gap: 10px; margin-top: 10px; }
.secret-grid > div, .endpoint-band > div { display: flex; align-items: center; gap: 8px; min-width: 0; }
.secret-grid small, .endpoint-band strong { width: 126px; color: var(--muted); flex: 0 0 auto; }
code { font-family: var(--mono); word-break: break-all; }
.endpoint-band { display: grid; gap: 9px; margin-bottom: 14px; padding: 14px 16px; border: 1px solid var(--line); background: var(--panel); }
.uri-list { display: grid; gap: 4px; font-size: 12px; }
.provider-tags, .row-actions { display: flex; align-items: center; gap: 5px; flex-wrap: wrap; }
</style>
