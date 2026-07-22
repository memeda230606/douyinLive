import { CheckCircle2, Database, FileJson, LockKeyhole, MonitorCog } from 'lucide-react'

import type { BootstrapDTO } from '../../app/bootstrap'

export function DiagnosticsPage({ bootstrap }: { bootstrap: BootstrapDTO }) {
  const checks = [
    { label: '桌面生命周期', value: bootstrap.state === 'RUNNING' ? '运行中' : bootstrap.state, icon: MonitorCog, healthy: bootstrap.state === 'RUNNING' },
    { label: 'SQLite 数据库', value: bootstrap.data.ready ? `Schema v${bootstrap.data.schemaVersion}` : '未就绪', icon: Database, healthy: bootstrap.data.ready },
    { label: '结构化日志', value: bootstrap.data.loggingReady ? 'JSONL 已启用' : '未就绪', icon: FileJson, healthy: bootstrap.data.loggingReady },
    { label: '凭据边界', value: 'Cookie 不回显', icon: LockKeyhole, healthy: true },
  ]
  return (
    <main className="page page--narrow">
      <div className="page__heading"><div><p className="eyebrow">运行健康</p><h1>诊断</h1><p>查看本地服务基础状态；日志和诊断数据默认执行敏感信息过滤。</p></div><div className="ready-badge"><CheckCircle2 aria-hidden="true" />基础检查完成</div></div>
      <section className="diagnostic-grid">{checks.map(({ label, value, icon: Icon, healthy }) => <article className="diagnostic-card" key={label}><Icon aria-hidden="true" /><div><span>{label}</span><strong>{value}</strong></div><i className={healthy ? 'health-dot health-dot--ok' : 'health-dot'} aria-label={healthy ? '正常' : '异常'} /></article>)}</section>
      <section className="settings-section">
        <div className="settings-section__heading"><LockKeyhole aria-hidden="true" /><div><h2>诊断隐私边界</h2><p>当前基础诊断只展示服务状态，不显示 Cookie、凭据引用、签名、完整流地址或原始 Protobuf。</p></div></div>
        <dl className="diagnostic-details">
          <div><dt>应用版本</dt><dd>{bootstrap.build.productVersion}</dd></div>
          <div><dt>Git 提交</dt><dd>{bootstrap.build.gitCommit}</dd></div>
          <div><dt>构建时间</dt><dd>{bootstrap.build.buildTime}</dd></div>
          <div><dt>构建来源</dt><dd>{bootstrap.build.buildSource}</dd></div>
          <div><dt>Go / Wails / Node</dt><dd>{bootstrap.build.goVersion} / {bootstrap.build.wailsVersion} / {bootstrap.build.nodeVersion}</dd></div>
          <div><dt>FFmpeg</dt><dd>{bootstrap.build.ffmpegVersion}</dd></div>
          <div><dt>FFmpeg SHA-256</dt><dd>{bootstrap.build.ffmpegSHA256}</dd></div>
          <div><dt>FFmpeg 许可证</dt><dd>{bootstrap.build.ffmpegLicense}</dd></div>
          <div><dt>数据库 / 设置 Schema</dt><dd>v{bootstrap.build.databaseSchemaVersion} / v{bootstrap.build.settingsSchemaVersion}</dd></div>
          <div><dt>分析 / 导出 Schema</dt><dd>{bootstrap.build.analysisAlgorithmVersion} / {bootstrap.build.exportSchemaVersion}</dd></div>
          <div><dt>API 契约</dt><dd>{bootstrap.apiVersion}</dd></div>
          <div><dt>数据模式</dt><dd>{bootstrap.data.mode}</dd></div>
          <div><dt>日志状态</dt><dd>{bootstrap.data.loggingReady ? '可用' : '不可用'}</dd></div>
        </dl>
      </section>
      <section className="empty-panel empty-panel--compact"><div><h2>诊断事件查询将在后续开放</h2><p>当前错误会写入本地脱敏 JSONL；完整筛选和诊断包导出属于后续诊断服务任务。</p></div></section>
    </main>
  )
}
