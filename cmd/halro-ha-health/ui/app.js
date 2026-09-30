    const names = {healthy:'健康',degraded:'降级',critical:'危险',unknown:'未知'};
    const reasonText = {
      'client probe not observed':'客户端入口探针未观测到结果',
      'client Service probe not configured':'客户端 Service 探针未配置',
      'client Service reached a different cluster':'客户端 Service 路由到其他集群',
      'client Service returned a route without cluster identity':'客户端 Service 路由缺少集群身份',
      'client Service reached Primary':'客户端 Service 已到达 Primary',
      'client Service probe transport unavailable':'客户端 Service 探针传输不可用',
      'client Service returned unexpected responses':'客户端 Service 返回非预期响应',
      'client Service returned only Replica routes within probe budget':'探针预算内仅到达 Replica',
      'conflicting observations for one member identity':'同一成员身份的观测互相矛盾',
      'member-reported identity differs from scrape inventory':'成员自报身份与采集清单不一致',
      'members report different incarnations':'成员自报的 incarnation 不一致',
      'a member promised a term below its current term':'成员的 promised term 低于当前 term',
      'a member reports invalid index ordering':'成员水位顺序不合法',
      'multiple members report Primary':'多个成员自报 Primary',
      'unconfigured HA member observed':'观测到清单外 HA 成员',
      'a member promised a term above the observed Primary':'成员的 promised term 高于已观测 Primary',
      'unconfigured HA member evidence is stale or incomplete':'清单外成员证据陈旧或不完整',
      'member coverage or identity is incomplete':'成员覆盖或身份不完整',
      'member role observation is incomplete':'成员角色观测不完整',
      'no member reports Primary':'没有成员自报 Primary',
      'Primary term is missing':'Primary term 缺失',
      'member observed an incompatible peer':'成员观测到不兼容 Peer',
      'member reports maintenance, startup, or replication unavailability':'成员报告维护、启动或复制不可用',
      'member safety evidence is incomplete':'成员安全证据不完整',
      'member is awaiting a role decision':'成员正在等待角色裁决',
      'member HA signal inventory is incomplete':'成员 HA 信号清单不完整',
      'Replica applied index exceeds the observed Primary confirmed index':'Replica 应用水位超过已观测 Primary 确认水位',
      'one observed Primary and consistent member identities':'已观测到唯一 Primary，成员身份一致',
      'Primary reports an internal replication block':'Primary 报告内部复制阻断',
      'Primary confirmation evidence is incomplete':'Primary 确认证据不完整',
      'Primary confirmation evidence is ambiguous':'Primary 确认证据互相矛盾',
      'Primary has not completed startup adjudication':'Primary 尚未完成启动裁决',
      'peer sessions were not observed':'未观测到 Peer 会话',
      'peer session observation is missing':'Peer 会话观测缺失',
      'Primary has no authenticated Replica session':'Primary 没有已认证的 Replica 会话',
      'no recent required confirmation success observed':'近五分钟没有必需确认写成功证据',
      'Primary recently completed a required confirmation barrier':'Primary 近期完成必需确认屏障',
      'Replica coverage is incomplete':'Replica 覆盖不完整',
      'Primary progress evidence is ambiguous':'Primary 进度证据互相矛盾',
      'Replica progress evidence is incomplete':'Replica 进度证据不完整',
      'Primary confirmed index is missing':'Primary 确认水位缺失',
      'no Replica role observed':'未观测到 Replica 角色',
      'at least one Replica has an observed matching index':'至少一个 Replica 的观测水位数值匹配',
      'no Replica has an observed matching index':'没有 Replica 的观测水位数值匹配',
      'no health evidence':'没有健康证据',
      'authenticated ordering prefixes differ at the same index':'相同 index 的认证 ordering 前缀不一致',
      'event archive member collection incomplete':'事件留存的成员采集不完整',
      'durable transition coverage incomplete':'持久迁移证据覆盖不完整',
      'current HA alerts unavailable':'当前 HA 告警不可读取',
    };
    const reasonPrefixes = [
      ['member machine status disagrees with fresh Metrics: ','机器状态与新鲜 Metrics 不一致：'],
      ['member health probe incomplete: ','成员健康探针不完整：'],
      ['member liveness probe failed: ','成员存活探针失败：'],
      ['Primary readiness probe failed: ','Primary 就绪探针失败：'],
      ['member machine status incomplete: ','成员机器状态不完整：'],
      ['event archive unavailable: ','事件留存不可用：'],
      ['firing alert: ','正在触发的告警：'],
    ];
    function displayReason(reason) {
      if(!reason) return '无有效数据';
      if(reasonText[reason]) return reasonText[reason];
      for(const [prefix,translation] of reasonPrefixes) {
        if(reason.startsWith(prefix)) return translation+reason.slice(prefix.length);
      }
      return reason;
    }
    const colors = ['#5cc8ff','#77e4ad','#ffcb78','#d4a6ff','#f482a5','#b7c7e7'];
    const byId = id => document.getElementById(id);
    function verifiedRunbookBase(value) {
      try {
        const url=new URL(value);
        return url.protocol==='https:'&&url.pathname==='/'&&!url.username&&!url.password&&!url.search&&!url.hash?url.href:'';
      } catch { return ''; }
    }
    const runbookBase=verifiedRunbookBase(document.body.dataset.runbookBase);
    if(runbookBase) {
      for(const link of document.querySelectorAll('.runbook-context')) {
        link.href=runbookBase+'docs/runbooks/ha-operations.md';link.hidden=false;
      }
      byId('runbook-path').hidden=true;
    }
    let currentMembers=[], currentStatuses=[], healthServerAt=NaN, healthRequestMono=NaN;
    function renderSignal(id, signal) {
      const card = byId(id), level = names[signal?.level] ? signal.level : 'unknown';
      card.className = 'card ' + level;
      card.querySelector('.status').textContent = names[level];
      card.querySelector('.reason').textContent = displayReason(signal?.reason);
    }
    function cell(row, value) { const td = document.createElement('td'); td.textContent = value == null || value === '' ? '未知' : String(value); row.append(td); }
    function ageMs(member) {
      const sample=Date.parse(member?.sampled_at),elapsed=performance.now()-healthRequestMono;
      return [sample,healthServerAt,elapsed].every(Number.isFinite)&&elapsed>=0?healthServerAt-sample+elapsed:NaN;
    }
    function scrapeAligned(member) {
      const oldest=Date.parse(member?.sampled_at),up=Date.parse(member?.up_sampled_at),newest=Date.parse(member?.newest_sampled_at);
      return [oldest,up,newest].every(Number.isFinite)&&Math.abs(up-oldest)<=1000&&Math.abs(newest-up)<=1000;
    }
    function fresh(member) { const age=ageMs(member); return member?.up===true&&Number.isFinite(age)&&age>=0&&age<=30000&&scrapeAligned(member); }
    function observation(member) {
      if(member?.up===false) return '失败';
      const age=ageMs(member);
      if(member?.up!==true||!Number.isFinite(age)||age<0||age>30000) return '缺失/陈旧';
      return !scrapeAligned(member)?'本轮指标缺报':member.conflicted?'矛盾':'新鲜';
    }
    function flag(value) { return value == null ? '未知' : value ? '是' : '否'; }
    function difference(a,b) { return Number.isSafeInteger(a)&&Number.isSafeInteger(b)?a-b:null; }
    function missingHASignals(member) {
      const missing=[];
      for(const [field,label] of [['maintenance','维护'],['startup_ready','启动裁决'],['replication_unavailable','复制阻断']]) {
        if(member[field]==null) missing.push(label);
      }
      for(const reason of ['schema','key_challenge','spki']) {
        if(member.incompatible_reasons?.[reason]==null) missing.push('不兼容原因 '+reason);
      }
      for(const other of currentMembers) {
        if(other.instance!==member.instance&&member.peers?.[other.instance]==null) missing.push('Peer '+other.instance);
      }
      return missing.length?missing.join('；'):'无';
    }
    function showDetail(member) {
      const machine=currentStatuses.find(status=>status.node_id===member.instance);
      byId('detail-title').textContent='节点详情 · '+member.instance;
      const list=byId('detail-list');list.replaceChildren();
      const add=(label,value)=>{const term=document.createElement('dt'),description=document.createElement('dd');term.textContent=label;description.textContent=value == null || value === ''?'未知':String(value);list.append(term,description);};
      add('指标来源','Prometheus /metrics');add('指标采集状态',observation(member));add('最早指标观测',member.sampled_at?new Date(member.sampled_at).toLocaleString('zh-CN'):null);
      add('本轮 up 观测',member.up_sampled_at?new Date(member.up_sampled_at).toLocaleString('zh-CN'):null);
      add('最晚指标观测',member.newest_sampled_at?new Date(member.newest_sampled_at).toLocaleString('zh-CN'):null);
      add('机器状态采集',machine?(machine.error?'未知：'+machine.error:'成功'):'未配置');
      add('逐节点 HTTP live',machine?flag(machine.health_live):null);
      add('逐节点 HTTP ready',machine?flag(machine.health_ready):null);
      add('逐节点健康探针异常',machine?.health_probe_error);
      if(machine&&!machine.error){add('状态采集时间',new Date(machine.sampled_at).toLocaleString('zh-CN'));add('认证状态角色 / term',machine.role+' / '+machine.term);add('元数据投影',machine.projection?JSON.stringify(machine.projection):null);}
      if(machine?.live_transitions){add('本进程迁移记录',`${machine.live_transitions.events?.length||0} 条 · 已淘汰 ${machine.live_transitions.dropped||0} 条 · 发布器启动 ${new Date(machine.live_transitions.publisher_started_at).toLocaleString('zh-CN')}`);}
      if(machine?.availability_transitions){add('Primary 可用性事件',`${machine.availability_transitions.current} · ${machine.availability_transitions.events?.length||0} 条 · 已淘汰 ${machine.availability_transitions.dropped||0} 条`);}
      if(machine?.prefix_digest) add('认证前缀摘要',`index ${machine.prefix_digest.index} · last frame term ${machine.prefix_digest.last_frame_term} · SHA-256 ${machine.prefix_digest.ordering_head_sha256}`);
      add('自报 Cluster ID',member.cluster_id);add('自报 Node ID',member.node_id);
      add('角色',member.role);add('Incarnation',member.incarnation);add('Term',member.term);add('Promised term',member.promised_term);
      add('持久 / 确认 / 应用',[(member.durable??'未知'),(member.confirmed??'未知'),(member.applied??'未知')].join(' / '));
      add('启动裁决完成',flag(member.startup_ready));
      add('复制内部阻断',flag(member.replication_unavailable));
      add('维护状态',flag(member.maintenance));add('曾观测不兼容',flag(member.incompatible));add('矛盾序列',flag(member.conflicted));
      add('HA 指标缺报',missingHASignals(member));
      add('认证 Peer 会话',member.peers?Object.entries(member.peers).map(([peer,connected])=>peer+': '+(connected==null?'未知':connected?'已连接':'断开')).join('；')||'无':null);
      byId('node-detail').showModal();
    }
    function renderTopology(members) {
      const topology=byId('topology');topology.replaceChildren();
      const primaries=(members||[]).filter(m=>fresh(m)&&m.role==='primary'&&!m.conflicted);
      const primary=primaries.length===1?primaries[0]:null;
      for(const m of members||[]) {
        const node=document.createElement('article');node.className='topology-node '+(m.up===false?'down':!fresh(m)?'stale':m.conflicted?'unknown':m.role==='primary'?'primary':'');
        const name=document.createElement('strong');name.textContent=m.instance;node.append(name);
        const role=document.createElement('small');role.textContent=(m.role||'角色未知')+' · '+observation(m);node.append(role);
        const link=document.createElement('small');
        link.textContent=!fresh(m)?'认证连接：当前未观测':m.role==='primary'?'到 Replica 的会话见下表':primary?.peers?.[m.instance]===true?'Primary → 本节点：已认证':primary?.peers?.[m.instance]===false?'Primary → 本节点：未连接':'Primary → 本节点：未知';
        node.append(link);topology.append(node);
      }
      byId('topology-note').textContent=primary?'唯一性由“安全一致性”卡判定；这里的箭头不证明应用水位或提升资格。':'没有可据以绘制连接的 Primary 观测。';
    }
    function renderNodes(members) {
      const tbody = byId('nodes'); tbody.replaceChildren();
      const primaries=(members||[]).filter(m=>fresh(m)&&m.role==='primary'&&!m.conflicted);
      const primary=primaries.length===1?primaries[0]:null;
      const ordered=[...(members||[])];
      const sort=byId('node-sort').value;
      ordered.sort((a,b)=>{
        if(sort==='role') return (a.role==='primary'?0:1)-(b.role==='primary'?0:1)||a.instance.localeCompare(b.instance);
        if(sort==='freshness') return observation(a).localeCompare(observation(b),'zh-CN')||a.instance.localeCompare(b.instance);
        if(sort==='backlog') return (difference(b.durable,b.applied)??-1)-(difference(a.durable,a.applied)??-1)||a.instance.localeCompare(b.instance);
        return a.instance.localeCompare(b.instance);
      });
      for (const m of ordered) {
        const row = document.createElement('tr');
        const nameCell=document.createElement('td'),button=document.createElement('button');button.type='button';button.className='node-button';button.textContent=m.instance;button.addEventListener('click',()=>showDetail(m));nameCell.append(button);row.append(nameCell);
        const machine=currentStatuses.find(status=>status.node_id===m.instance);
        const pageElapsed=performance.now()-healthRequestMono;
        const pageCurrent=Number.isFinite(healthServerAt)&&Number.isFinite(pageElapsed)&&pageElapsed>=0&&pageElapsed<=30000;
        cell(row,observation(m));cell(row,!pageCurrent?'状态陈旧':machine?(machine.error?'未知：'+machine.error:machine.health_probe_error?'状态成功；健康探针异常：'+machine.health_probe_error:'成功'):'未配置');
        cell(row,pageCurrent&&machine?flag(machine.health_live):null);cell(row,pageCurrent&&machine?flag(machine.health_ready):null);
        cell(row,m.role);cell(row,m.incarnation);
        cell(row,(m.term ?? '未知') + ' / ' + (m.promised_term ?? '未知'));
        cell(row,m.durable); cell(row,m.confirmed); cell(row,m.applied);
        cell(row,fresh(m)?difference(m.durable,m.confirmed):null);
        cell(row,fresh(m)?difference(m.durable,m.applied):null);
        cell(row,fresh(m)?difference(m.confirmed,m.applied):null);
        const comparable=m.role==='replica'&&primary&&fresh(m)&&primary.incarnation===m.incarnation&&primary.term===m.term&&Math.abs(Date.parse(primary.sampled_at)-Date.parse(m.sampled_at))<=30000;
        cell(row,comparable?difference(primary.confirmed,m.applied):null);
        cell(row,flag(m.startup_ready));cell(row,flag(m.replication_unavailable));cell(row,flag(m.incompatible));
        cell(row,!fresh(m)?'未知（指标陈旧）':m.peers?Object.entries(m.peers).map(([peer,connected])=>peer+':'+(connected==null?'未知':connected?'已连接':'断开')).join('，')||'无':'未知');
        cell(row,flag(m.maintenance));
        cell(row,m.sampled_at ? new Date(m.sampled_at).toLocaleString('zh-CN') : '未知'); tbody.append(row);
      }
    }
    function renderAlerts(alerts) {
      const tbody=byId('alerts');tbody.replaceChildren();
      if(alerts===null) { const row=document.createElement('tr');cell(row,'当前告警查询失败');tbody.append(row);return; }
      if(!alerts?.length) { const row=document.createElement('tr');cell(row,'无当前 HA 告警');tbody.append(row);return; }
      for(const a of alerts) {
        const row=document.createElement('tr'),name=document.createElement('td'),button=document.createElement('button');
        button.type='button';button.className='node-button';button.textContent=a.name;
        button.addEventListener('click',()=>showAlertDetail(a));name.append(button);row.append(name);
        cell(row,a.severity);cell(row,a.instance||'集群');cell(row,a.state);
        cell(row,a.started?new Date(a.started).toLocaleString('zh-CN'):'未知');cell(row,a.summary);
        const runbook=document.createElement('td');
        if(runbookBase&&/^\/docs\/observability\/operations-runbook\.md#[a-z0-9-]+$/.test(a.runbook||'')) {
          const link=document.createElement('a');link.href=runbookBase+a.runbook.slice(1);link.textContent='查看处置';runbook.append(link);
        } else runbook.textContent=a.runbook||'未提供';
        row.append(runbook);tbody.append(row);
      }
    }
    let alertDetailGeneration=0;
    async function showAlertDetail(alert) {
      const generation=++alertDetailGeneration,dialog=byId('alert-detail'),list=byId('alert-detail-list');
      byId('alert-detail-title').textContent='告警证据 · '+alert.name;
      list.replaceChildren();
      const add=(label,value,formula=false)=>{
        const term=document.createElement('dt'),description=document.createElement('dd');
        term.textContent=label;description.textContent=value==null||value===''?'未知':String(value);
        if(formula) description.style.whiteSpace='pre-wrap';
        list.append(term,description);return description;
      };
      add('告警状态 / 级别',(alert.state||'未知')+' / '+(alert.severity||'未知'));
      add('影响节点',alert.instance||'集群范围，按表达式核对');
      add('活动开始',alert.started?new Date(alert.started).toLocaleString('zh-CN'):'未知');
      add('规则结果值',alert.value||'未提供；不是原始指标样本');
      add('摘要',alert.summary);
      const observed=alert.instance?currentMembers.filter(member=>member.instance===alert.instance):currentMembers;
      if(!observed.length) add('最近成员观测','当前页无对应成员样本');
      for(const member of observed) {
        add('成员 '+member.instance+' 最近观测',`${observation(member)} · ${member.sampled_at||'时间未知'} · ${member.role||'角色未知'} · incarnation ${member.incarnation||'未知'} · term ${member.term??'未知'} · durable/confirmed/applied ${member.durable??'未知'}/${member.confirmed??'未知'}/${member.applied??'未知'}`);
      }
      const ruleStatus=add('当前加载规则','正在查询 Prometheus');
      if(!dialog.open) dialog.showModal();
      const rule=await readJSON('/api/alert-rule?name='+encodeURIComponent(alert.name));
      if(generation!==alertDetailGeneration||!dialog.open) return;
      ruleStatus.textContent=rule?.status==='available'?'已读取':rule?.status==='ambiguous'?'同名规则不唯一':rule?.status==='missing'?'未找到规则':rule?.status==='invalid'?'规则内容无效':'规则查询失败';
      if(rule?.status==='available') {
        add('触发表达式（当前加载）',rule.query,true);
        add('持续条件 for',Number.isFinite(rule.for_seconds)?rule.for_seconds+' 秒':'未知');
        add('规则健康',rule.health||'未提供');
        add('最近规则求值',rule.last_evaluation||'未提供');
      }
    }
    function renderAlertHistory(series) {
      const tbody=byId('alert-history');tbody.replaceChildren();
      if(series===null) { const row=document.createElement('tr');cell(row,'历史查询失败');tbody.append(row);return; }
      const episodes=[];
      for(const item of series||[]) {
        if(item.metric?.__name__!=='ALERTS') continue;
        let episode=null;
        for(const [rawTime,rawValue] of item.values||[]) {
          const time=Number(rawTime),value=Number(rawValue);
          if(!Number.isFinite(time)||value!==1) continue;
          if(!episode||time-episode.last>45) {
            episode={name:item.metric.alertname,instance:item.metric.instance||'集群',severity:item.metric.severity,first:time,last:time,count:0};episodes.push(episode);
          }
          episode.last=time;episode.count++;
        }
      }
      episodes.sort((a,b)=>b.last-a.last);
      if(!episodes.length) { const row=document.createElement('tr');cell(row,'所选时间窗无可用告警采样');tbody.append(row);return; }
      for(const episode of episodes.slice(0,100)) {
        const row=document.createElement('tr');cell(row,episode.name);cell(row,episode.instance);cell(row,episode.severity);
        cell(row,new Date(episode.first*1000).toLocaleString('zh-CN'));cell(row,new Date(episode.last*1000).toLocaleString('zh-CN'));cell(row,episode.count);tbody.append(row);
      }
    }
    function renderEvents(events) {
      const tbody=byId('sampled-events');tbody.replaceChildren();
      if(events===null) { const row=document.createElement('tr');cell(row,'历史查询失败');tbody.append(row);return; }
      if(!events?.length) { const row=document.createElement('tr');cell(row,'所选时间窗无可判定的相邻采样变化');tbody.append(row);return; }
      const kinds={role:'角色',term:'Term',incarnation:'Incarnation',replication_phase:'复制阶段',maintenance:'维护',peer_session:'认证会话'};
      for(const event of events) {
        const row=document.createElement('tr');cell(row,event.instance);cell(row,kinds[event.kind]||event.kind);
        cell(row,event.peer||'—');cell(row,event.from);cell(row,event.to);cell(row,event.previous_seen?new Date(event.previous_seen).toLocaleString('zh-CN'):'未知');
        cell(row,event.first_seen?new Date(event.first_seen).toLocaleString('zh-CN'):'未知');tbody.append(row);
      }
    }
    function renderLiveTransitions(statuses) {
      const tbody=byId('live-transitions');tbody.replaceChildren();
      const summary=byId('live-transition-summary');
      const all=[];let available=0,dropped=0;
      for(const status of statuses||[]) {
        const history=status.error?'':status.live_transitions;
        if(!history) continue;
        available++;dropped+=history.dropped||0;
        for(const event of history.events||[]) all.push({node:status.node_id,event});
      }
      all.sort((a,b)=>Date.parse(b.event.at)-Date.parse(a.event.at));
      summary.textContent=`机器状态事件覆盖 ${available}/${statuses?.length||0} 个节点；各进程内已淘汰 ${dropped} 条。重启前历史未保留，离线提升可能只在重启后的状态中出现。`;
      if(!all.length){const row=document.createElement('tr');cell(row,available?'所采集进程暂无迁移记录':'真实迁移事件未观测');tbody.append(row);return;}
      for(const {node,event} of all.slice(0,200)) {
        const row=document.createElement('tr');cell(row,node);cell(row,event.kind);
        cell(row,`${event.from_role} / term ${event.from_term} / promise ${event.from_promised_term}`);
        cell(row,`${event.to_role} / term ${event.to_term} / promise ${event.to_promised_term}`);
        cell(row,new Date(event.at).toLocaleString('zh-CN'));cell(row,event.sequence);tbody.append(row);
      }
    }
    function renderAvailabilityTransitions(statuses) {
      const tbody=byId('availability-transitions');tbody.replaceChildren();
      const all=[];let available=0,dropped=0;
      for(const status of statuses||[]) {
        const history=status.error?'':status.availability_transitions;
        if(!history) continue;
        available++;dropped+=history.dropped||0;
        for(const event of history.events||[]) all.push({node:status.node_id,event});
      }
      all.sort((a,b)=>Date.parse(b.event.at)-Date.parse(a.event.at));
      byId('availability-transition-summary').textContent=`Primary coordinator 事件来源 ${available} 个；各进程内已淘汰 ${dropped} 条。Replica 不导出 Primary 确认能力事件；进程重启前的变化不在此表。`;
      if(!all.length){const row=document.createElement('tr');cell(row,available?'所采集进程暂无可用性切换':'可用性边界事件未观测');tbody.append(row);return;}
      for(const {node,event} of all.slice(0,200)) {
        const row=document.createElement('tr');cell(row,node);cell(row,event.from);cell(row,event.to);cell(row,event.reason);
        cell(row,new Date(event.at).toLocaleString('zh-CN'));cell(row,event.sequence);tbody.append(row);
      }
    }
    function renderReplicaStages(statuses) {
      const tbody=byId('replica-stage-transitions');tbody.replaceChildren();
      const all=[];let available=0,dropped=0;
      for(const status of statuses||[]) {
        const history=status.error?'':status.replica_stage_transitions;
        if(!history) continue;
        available++;dropped+=history.dropped||0;
        for(const event of history.events||[]) all.push({node:status.node_id,event});
      }
      all.sort((a,b)=>Date.parse(b.event.at)-Date.parse(a.event.at));
      byId('replica-stage-summary').textContent=`Replica 阶段来源 ${available} 个；各进程内已淘汰 ${dropped} 条。只记录失败/恢复边界，无事件不证明阶段健康；目标 index 不是持久水位。`;
      if(!all.length){const row=document.createElement('tr');cell(row,available?'所采集 Replica 暂无失败/恢复边界':'Replica 阶段事件未观测');tbody.append(row);return;}
      for(const {node,event} of all.slice(0,200)) {
        const row=document.createElement('tr');cell(row,node);cell(row,event.stage==='receive'?'接收':'应用');cell(row,event.from);cell(row,event.to);
        cell(row,event.reason);cell(row,event.target_index);cell(row,new Date(event.at).toLocaleString('zh-CN'));tbody.append(row);
      }
    }
    function renderEventArchive(archive) {
      const tbody=byId('event-archive');tbody.replaceChildren();
      const summary=byId('archive-summary');
      if(!archive) {
        summary.textContent='本次留存查询失败，无法判断事件文件状态。';
        const row=document.createElement('tr');cell(row,'查询失败');tbody.append(row);return;
      }
      if(archive.status==='not_configured') {
        summary.textContent='独立事件留存未配置；本进程事件仅在成员内存环中可见。';
        const row=document.createElement('tr');cell(row,'未配置');tbody.append(row);return;
      }
      const status={ok:'文件写入正常',partial:'成员采集不完整',stale:'采集陈旧',journal_write_failed:'文件写入失败'}[archive.status]||'未知';
      const failures=archive.collection_errors?.length?`；本轮成员采集失败：${archive.collection_errors.join('、')}`:'';
      summary.textContent=`留存状态：${status}；最近采集 ${archive.polled_at?new Date(archive.polled_at).toLocaleString('zh-CN'):'尚无'}${failures}；容量淘汰 ${archive.retention_dropped||0} 条。缺口标记表示无法证明事件完整性；本文件也不是 HA Audit。`;
      const labels={initial_observation:'首次观测：此前历史未知',source_changed:'事件源实例变化：间隔期间未知',incarnation_changed:'Incarnation 变化：前后证据分离',collection_failed:'采集失败：故障期间未知',collection_resumed:'首次恢复观测：故障期间仍未知',ring_history_missing:'内存环已淘汰：事件缺失',sequence_regressed:'同一事件源序号倒退：来源异常'};
      const records=(archive.records||[]).slice(-100).reverse();
      if(!records.length){const row=document.createElement('tr');cell(row,'尚无留存事件');tbody.append(row);return;}
      for(const record of records) {
        const row=document.createElement('tr'), event=record.live||record.availability||record.replica_stage;
        const source={state_publisher:'状态发布',primary_coordinator:'Primary 协调器',replica_stage:'Replica 阶段'};
        cell(row,record.node_id);cell(row,record.incarnation||'未知');cell(row,source[record.source]||record.source);
        cell(row,record.gap?'缺口':record.live?'迁移':record.replica_stage?'阶段':'可用性');
        cell(row,record.gap?(labels[record.gap]||record.gap):record.live?
          `${event.kind}: ${event.from_role} / ${event.from_term} → ${event.to_role} / ${event.to_term}`:
          record.replica_stage?`${event.stage}: ${event.from} → ${event.to} (${event.reason}, index ${event.target_index})`:
          `${event.from} → ${event.to} (${event.reason})`);
        cell(row,event?.at?new Date(event.at).toLocaleString('zh-CN'):'未知');
        cell(row,new Date(record.observed_at).toLocaleString('zh-CN'));tbody.append(row);
      }
    }
    function renderDurableArchive(archive) {
      const tbody=byId('durable-archive');tbody.replaceChildren();
      const eventsBody=byId('durable-events');eventsBody.replaceChildren();
      const summary=byId('durable-archive-summary');
      if(!archive) { summary.textContent='分页证据查询失败，覆盖未知。';const row=document.createElement('tr');cell(row,'查询失败');tbody.append(row);return; }
      if(archive.status==='not_configured') { summary.textContent='持久迁移证据采集未配置，不能声明跨重启角色迁移链完整。';const row=document.createElement('tr');cell(row,'未配置');tbody.append(row);return; }
      const labels={caught_up:'已追到观测头部',catching_up:'追赶中',not_started:'尚未采到基线',stale:'采集陈旧',unavailable:'来源不可用',journal_full:'文件已满',journal_write_failed:'文件写入失败'};
      const failures={journal_changed_requires_reconciliation:'成员日志已更换，旧链待核验交接；请保留采集器文件和旧成员数据'};
      const storage=Number.isFinite(archive.storage_bytes)?`${(archive.storage_bytes/1048576).toFixed(2)} MiB`:'未知';
      const manifest=Number.isFinite(archive.manifest_bytes)?`${(archive.manifest_bytes/4194304*100).toFixed(1)}%`:'未知';
      summary.textContent=`覆盖状态：${archive.status==='caught_up'?'全部成员最近一次采集已追到各自提交点':'不完整'}。本地证据 ${archive.closed_segments??0} 个已关闭段，共 ${storage}；当前清单占单文件上限 ${manifest}。仍需外部归档与容量监控。旧版本不可用，传统迁移基线之前的事件未知；此处不证明 fencing、quorum 或客户端恢复。`;
      for(const member of archive.members||[]) {
        const row=document.createElement('tr');
        cell(row,member.node_id);cell(row,member.incarnation||'未观测');cell(row,member.baseline_kind==='legacy_baseline'?'传统迁移基线（此前未知）':member.baseline_kind==='initial_state'?'初始状态':'未观测');
        cell(row,labels[member.status]||member.status);cell(row,member.stored_sequence);cell(row,member.observed_head);cell(row,member.events_retained);
        cell(row,member.observed_at?new Date(member.observed_at).toLocaleString('zh-CN'):'未观测');cell(row,failures[member.failure]||member.failure||'—');tbody.append(row);
      }
      const events=(archive.members||[]).flatMap(member=>(member.recent_events||[]).map(event=>({node:member.node_id,incarnation:member.incarnation,event})));
      events.sort((a,b)=>Date.parse(b.event.at)-Date.parse(a.event.at));
      if(!events.length) { const row=document.createElement('tr');cell(row,'无已留存迁移事件');eventsBody.append(row); }
      for(const {node,incarnation,event} of events) {
        const row=document.createElement('tr');cell(row,node);cell(row,incarnation);cell(row,event.kind);
        cell(row,`${event.from_role} / ${event.from_term} / ${event.from_promised_term}`);
        cell(row,`${event.to_role} / ${event.to_term} / ${event.to_promised_term}`);
        cell(row,new Date(event.at).toLocaleString('zh-CN'));cell(row,event.sequence);eventsBody.append(row);
      }
    }
    function renderStageLatencies(members, values) {
      const tbody=byId('stage-latencies');tbody.replaceChildren();
      const labels=['durable_to_confirm','sink_persist','apply_batch'];
      for(const member of members||[]) {
        const row=document.createElement('tr');cell(row,member.instance);
        for(const stage of labels) {
          const applicable=stage==='durable_to_confirm'?member.role==='primary':member.role==='replica';
          const item=values?.find(value=>value.instance===member.instance&&value.stage===stage);
          cell(row,values==null?'查询失败':!applicable?'不适用':item?`${(item.p95_seconds*1000).toFixed(1)} ms`:'无足够样本');
        }
        tbody.append(row);
      }
    }
    function renderConfirmationEvidence(evidence) {
      const panel=byId('confirmation-evidence');
      if(!evidence) {panel.textContent='当前健康查询失败，确认依据未知。';return;}
      const source='来源：'+(evidence.source||'未知')+'。';
      if(evidence.status==='observed'||evidence.status==='nonpositive') {
        const value=Number(evidence.increase_5m);
        const estimate=Number.isFinite(value)?value.toFixed(2):'未知';
        panel.textContent=`节点 ${evidence.instance||'未知'}；近 5 分钟成功等待的 Prometheus 估算增量 ${estimate}；两类计数中最旧的抓取 ${evidence.sampled_at?new Date(evidence.sampled_at).toLocaleString('zh-CN'):'未知'}；稳定任期规则抓取 ${evidence.epoch_sampled_at?new Date(evidence.epoch_sampled_at).toLocaleString('zh-CN'):'未知'}；窗口求值 ${evidence.evaluated_at?new Date(evidence.evaluated_at).toLocaleString('zh-CN'):'未知'}。${source}此值只证明内部必需确认屏障的观察结果，不等于客户端完整成功。`;
        return;
      }
      const reasons={query_failed:'查询失败',ambiguous:'候选序列互相重叠',no_fresh_sample:'没有新鲜的稳定任期成功等待样本'};
      panel.textContent=`确认依据未知：${reasons[evidence.status]||'来源状态不明'}。${source}空闲时没有新写也可能没有正向样本。`;
    }
    function svg(tag, attrs) { const el = document.createElementNS('http://www.w3.org/2000/svg', tag); for (const [k,v] of Object.entries(attrs)) el.setAttribute(k,String(v)); return el; }
    function renderChart(series, events) {
      const chart=byId('chart'), legend=byId('legend'); chart.replaceChildren(); legend.replaceChildren();
      if(series===null) { chart.append(svg('text',{x:25,y:130,fill:'#b7c5d9'}));chart.firstChild.textContent='历史查询失败';return; }
      const lines=[], terms=new Map(), incarnations=new Map();
      for (const item of series || []) {
        const values=(item.values || []).map(([t,v])=>[Number(t),Number(v)]).filter(([t,v])=>Number.isFinite(t)&&Number.isFinite(v));
        const instance=item.metric?.instance||'?';
        if(item.metric?.__name__==='halro_cluster_term') {
          terms.set(instance,new Map((item.values||[]).map(([t,v])=>[Number(t),String(v)]).filter(([t])=>Number.isFinite(t))));
        }
        if(item.metric?.__name__==='halro_cluster_incarnation_info') {
          const current=incarnations.get(instance)||new Map();
          for(const [t,v] of values) if(v===1) {
            const prior=current.get(t), next=item.metric.incarnation;
            current.set(t,prior&&prior!==next?'冲突':next);
          }
          incarnations.set(instance,current);
        }
        if(item.metric?.__name__==='halro_replication_index') {
          const exact=values.filter(([,v])=>Number.isSafeInteger(v)&&v>=0);
          if(exact.length) lines.push({instance,name:instance+' '+(item.metric?.kind||'?'),values:exact});
        }
      }
      if (!lines.length) { chart.append(svg('text',{x:25,y:130,fill:'#b7c5d9'})); chart.firstChild.textContent='无历史水位样本'; return; }
      let minT=Infinity,maxT=-Infinity,minV=Infinity,maxV=-Infinity;
      for(const line of lines) for(const [t,v] of line.values) {minT=Math.min(minT,t);maxT=Math.max(maxT,t);minV=Math.min(minV,v);maxV=Math.max(maxV,v);}
      const x=t=>55+(t-minT)/Math.max(1,maxT-minT)*815, y=v=>220-(v-minV)/Math.max(1,maxV-minV)*180;
      chart.append(svg('line',{x1:55,y1:220,x2:875,y2:220,stroke:'#6b819f'}));
      const low=svg('text',{x:5,y:220,fill:'#a9b9d1'});low.textContent=String(minV);chart.append(low);
      const high=svg('text',{x:5,y:40,fill:'#a9b9d1'});high.textContent=String(maxV);chart.append(high);
      const timeLabel=t=>new Date(t*1000).toLocaleString('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'});
      const start=svg('text',{x:55,y:248,fill:'#a9b9d1','font-size':12});start.textContent=timeLabel(minT);chart.append(start);
      const end=svg('text',{x:875,y:248,fill:'#a9b9d1','font-size':12,'text-anchor':'end'});end.textContent=timeLabel(maxT);chart.append(end);
      const markerKinds={role:'角色',term:'Term',incarnation:'Incarnation',replication_phase:'复制阶段',maintenance:'维护',peer_session:'Peer 会话'};
      const markerGroups=new Map();
      for(const event of (events||[]).slice(0,80)) {
        const t=Date.parse(event.first_seen)/1000;
        if(!markerKinds[event.kind]||!Number.isFinite(t)||t<minT||t>maxT) continue;
        const key=event.instance+':'+t, group=markerGroups.get(key)||{t,labels:[],kinds:new Set()};
        group.labels.push(`${event.instance}${event.peer?' → '+event.peer:''} ${markerKinds[event.kind]}: ${event.from} → ${event.to}（首次观测）`);
        group.kinds.add(event.kind);markerGroups.set(key,group);
      }
      for(const group of markerGroups.values()) {
        const color=group.kinds.has('maintenance')?'#ffcf75':group.kinds.has('peer_session')?'#ff8f9a':group.kinds.has('role')?'#5cc8ff':'#d5def1';
        const marker=svg('line',{x1:x(group.t),y1:28,x2:x(group.t),y2:220,stroke:color,'stroke-dasharray':'4 4','stroke-width':1});
        const title=svg('title',{});title.textContent=group.labels.join('；');marker.append(title);chart.append(marker);
      }
      lines.forEach((line,i)=>{
        const color=colors[i%colors.length];let segment=[],previous=null;
        const draw=()=>{
          if(segment.length===1) {
            const dot=svg('circle',{cx:x(segment[0][0]),cy:y(segment[0][1]),r:3.5,fill:color});
            const title=svg('title',{});title.textContent=line.name+' · '+new Date(segment[0][0]*1000).toLocaleString('zh-CN')+' · index '+segment[0][1];
            dot.append(title);chart.append(dot);
          } else if(segment.length>1) {
            const points=[segment[0]];
            for(let j=1;j<segment.length;j++) {points.push([segment[j][0],segment[j-1][1]],segment[j]);}
            chart.append(svg('polyline',{points:points.map(([t,v])=>x(t)+','+y(v)).join(' '),fill:'none',stroke:color,'stroke-width':2}));
          }
          segment=[];
        };
        for(const point of line.values) {
          const [t]=point, term=terms.get(line.instance)?.get(t), incarnation=incarnations.get(line.instance)?.get(t);
          if(previous&&(t-previous.t>45||term!==previous.term||incarnation!==previous.incarnation))draw();
          segment.push(point);previous={t,term,incarnation};
        }
        draw();
        const label=document.createElement('span'), dot=document.createElement('i'); dot.style.background=color; label.append(dot,document.createTextNode(line.name)); legend.append(label);
      });
      const markerLabel=document.createElement('span');markerLabel.textContent='虚线：采样变化（最多 80 组，悬停查看）';legend.append(markerLabel);
    }
    async function readJSON(path) {
      try {
        const response=await fetch(path,{cache:'no-store'});
        return response.ok?await response.json():null;
      } catch { return null; }
    }
    let refreshGeneration=0,historyGeneration=0,lastHistoricalPollMono=-Infinity,lastHealthRenderedAt=0,lastHealthRenderedMono=NaN,healthExpiryTimer=null;
    function stopHealthExpiryTimer() {
      if(healthExpiryTimer!==null) {clearTimeout(healthExpiryTimer);healthExpiryTimer=null;}
    }
    function expireHealth() {
      if(!lastHealthRenderedAt||!Number.isFinite(lastHealthRenderedMono)||performance.now()-lastHealthRenderedMono<=30000) return;
      stopHealthExpiryTimer();
      for(const id of ['overall','client','confirmation','safety','catchup']) {
        renderSignal(id,{level:'unknown',reason:'当前健康查询超过 30 秒未更新'});
      }
      byId('updated').textContent='最近成功查询：'+new Date(lastHealthRenderedAt).toLocaleString('zh-CN')+' · 当前状态已过期';
      lastHealthRenderedAt=0;lastHealthRenderedMono=NaN;healthServerAt=NaN;healthRequestMono=NaN;
      byId('scope').textContent='集群身份与采集覆盖未知 · 最近一次健康查询已过期';
      byId('unexpected-members').hidden=true;
      byId('unexpected-members').textContent='';
      renderTopology(currentMembers);renderNodes(currentMembers);renderConfirmationEvidence(null);
    }
    async function refresh(forceHistory=false) {
      expireHealth();
      const generation=++refreshGeneration, historical=byId('view').value==='history';
      const pollHistory=historical&&(forceHistory||performance.now()-lastHistoricalPollMono>=60000);
      if(pollHistory) lastHistoricalPollMono=performance.now();
      const historyRequest=pollHistory?++historyGeneration:historyGeneration;
      const details=Promise.all([
        pollHistory?readJSON('/api/history?minutes='+byId('window').value):undefined,
        historical?undefined:readJSON('/api/alerts'),
        historical?undefined:readJSON('/api/latency'),
        historical?undefined:readJSON('/api/impact'),
        pollHistory?readJSON('/api/event-archive'):undefined,
        historical?undefined:readJSON('/api/client-final'),
        pollHistory?readJSON('/api/durable-transitions'):undefined,
      ]);
      const requestStarted=performance.now();
      let state=await readJSON('/api/health');
      const expiredResponse=performance.now()-requestStarted>30000;
      const invalidObservedAt=state&&!Number.isFinite(Date.parse(state.observed_at));
      if(expiredResponse||invalidObservedAt) state=null;
      if(generation===refreshGeneration) {
        const error=byId('error');
        if(state) {
          stopHealthExpiryTimer();
          lastHealthRenderedAt=Date.now();lastHealthRenderedMono=requestStarted;
          healthServerAt=Date.parse(state.observed_at);healthRequestMono=requestStarted;
          healthExpiryTimer=setTimeout(expireHealth,Math.max(1,30001-(performance.now()-requestStarted)));
          error.hidden=true;
          for(const id of ['overall','client','confirmation','safety','catchup']) renderSignal(id,state[id]);
          currentMembers=state.members||[];currentStatuses=state.member_statuses||[];
          byId('two-node').hidden=state.expected_members!==2;
          const freshCount=currentMembers.filter(fresh).length;
          const incarnations=[...new Set(currentMembers.map(m=>m.incarnation).filter(Boolean))];
          const identity=incarnations.length===1?incarnations[0]:incarnations.length>1?'冲突':'未知';
          const unexpected=Array.isArray(state.unexpected_members)?state.unexpected_members:[];
          const uncertain=unexpected.filter(member=>!member.observed).length;
          byId('scope').textContent='环境 '+state.environment+' · 集群 '+state.cluster+' · Incarnation '+identity+' · 采集 '+freshCount+'/'+state.expected_members+(unexpected.length?' · 异常来源 '+unexpected.length+'（未证实 '+uncertain+'）':'');
          const unexpectedNote=byId('unexpected-members');unexpectedNote.hidden=!unexpected.length;
          unexpectedNote.textContent=unexpected.length?'异常来源证据：'+unexpected.slice(0,10).map(member=>(member.identity_missing?'缺少 instance 标签':(member.instance||'未知'))+' '+(member.identity_missing?'身份不可核对':member.observed?'近期已观测':'未证实/陈旧')+' · '+(member.sampled_at||'无有效时间')).join('；')+(unexpected.length>10?'；另有 '+(unexpected.length-10)+' 个':''):'';
          byId('updated').textContent='服务端观测：'+new Date(state.observed_at).toLocaleString('zh-CN')+' · 页面更新：'+new Date().toLocaleString('zh-CN');
        } else {
          stopHealthExpiryTimer();
          lastHealthRenderedAt=0;lastHealthRenderedMono=NaN;healthServerAt=NaN;healthRequestMono=NaN;
          const failureReason=expiredResponse?'本次查询过期':invalidObservedAt?'观测时间无效':'本次查询失败';
          for(const id of ['overall','client','confirmation','safety','catchup']) renderSignal(id,{level:'unknown',reason:failureReason});
          currentMembers=[];currentStatuses=[];
          error.textContent=expiredResponse?'当前健康查询返回时已过期；请重新查询。':invalidObservedAt?'当前健康查询缺少有效的服务端观测时间。':'当前健康查询失败；历史证据如可用仍单独显示。';error.hidden=false;
          byId('two-node').hidden=true;
          byId('scope').textContent='集群身份与采集覆盖未知';
          byId('unexpected-members').hidden=true;byId('unexpected-members').textContent='';
          byId('updated').textContent='当前状态不可用：'+new Date().toLocaleString('zh-CN');
        }
      }
      const [historyData,currentAlerts,latency,impact,archive,clientFinal,durableArchive]=await details;
      if(historical) {
        if(pollHistory&&historyRequest===historyGeneration&&byId('view').value==='history') {
          const series=Array.isArray(historyData?.series)?historyData.series:null;
          const events=Array.isArray(historyData?.sampled_events)?historyData.sampled_events:null;
          renderChart(series,events);renderEvents(events);renderAlertHistory(series);renderEventArchive(archive);renderDurableArchive(durableArchive);
        }
        return;
      }
      if(generation!==refreshGeneration||byId('view').value!=='current') return;
      renderTopology(currentMembers);renderNodes(currentMembers);
      renderConfirmationEvidence(state?.confirmation_evidence);
      renderLiveTransitions(currentStatuses);renderAvailabilityTransitions(currentStatuses);renderReplicaStages(currentStatuses);
      renderStageLatencies(currentMembers,latency);renderAlerts(currentAlerts);
      const primary=currentMembers.find(m=>fresh(m)&&m.role==='primary'&&!m.conflicted);
      const client='客户端入口：'+(names[state?.client?.level]||'未知')+'；Primary 内部复制阻断：'+flag(primary?.replication_unavailable)+'。';
      if(impact?.observed){
        const count=name=>Number(impact.counts?.[name]??0).toFixed(1);
        byId('impact').textContent='近 5 分钟服务端写路由响应（Prometheus 估算）：2xx '+count('http_2xx')+'，4xx '+count('http_4xx')+'，非 Primary '+count('not_primary')+'，503 '+count('http_503')+'，超时 '+count('timeout')+'，其他 5xx '+count('http_5xx')+'，取消 '+count('canceled')+'，写出失败 '+count('write_error')+'。'+client+'重试会重复计数；503 不一定由复制阻断造成，流式 2xx 可能随后失败，不能当作客户端完整成功。';
      } else {
        byId('impact').textContent='写路由 HTTP 结果：查询失败、成员指标缺失或尚无完整采样，未观测。'+client+'不能由内部复制状态推算请求成功率。';
      }
      const finalPanel=byId('client-final');
      if(clientFinal?.status==='observed'&&clientFinal.counts&&typeof clientFinal.counts==='object') {
        const classes=Object.keys(clientFinal.counts).sort();
        finalPanel.textContent='近 5 分钟客户端最终结果（Prometheus 估算）：'+classes.map(name=>{
          const values=clientFinal.counts[name]||{};
          return name+'：成功 '+Number(values.success||0).toFixed(1)+'，拒绝 '+Number(values.rejected||0).toFixed(1)+'，超时 '+Number(values.timeout||0).toFixed(1)+'，传输失败 '+Number(values.transport_failure||0).toFixed(1)+'，取消 '+Number(values.canceled||0).toFixed(1)+'，流不完整 '+Number(values.incomplete_stream||0).toFixed(1);
        }).join('；')+'。按部署清单声明的外部客户端结果来源估算；仅覆盖清单中的操作类别与观测者。部署声明 ID：'+(clientFinal.acceptance_record||'缺失')+'；完整回执和流量覆盖须查目标环境验收记录。';
      } else {
        const finalReasons={query_failed:'Prometheus 查询失败',coverage_incomplete:'观测者覆盖或采样连续性不完整',empty_window:'窗口内无最终逻辑操作'};
        finalPanel.textContent=clientFinal?.status==='not_configured'?'客户端最终逻辑操作结果：未接入。':'客户端最终逻辑操作结果：未观测（'+(finalReasons[clientFinal?.reason]||'查询或来源未满足门限')+'）。';
      }
    }
    function setView(view) {
      const selected=view==='history'?'history':'current';
      byId('view').value=selected;
      for(const panel of document.querySelectorAll('[data-view]')) panel.hidden=panel.dataset.view!==selected;
    }
    byId('view').addEventListener('change',event=>{setView(event.target.value);refresh(true);});
    for(const card of document.querySelectorAll('.grid a[href^="#"]')) {
      card.addEventListener('click',()=>{
        const target=document.querySelector(card.getAttribute('href'))?.closest('[data-view]');
        if(target&&target.dataset.view!==byId('view').value){setView(target.dataset.view);refresh(true);}
      });
    }
    byId('refresh').addEventListener('click',()=>refresh(true));
    byId('window').addEventListener('change',()=>{byId('export').href='/api/evidence?minutes='+byId('window').value;refresh(true);});
    byId('node-sort').addEventListener('change',()=>renderNodes(currentMembers));
    byId('detail-close').addEventListener('click',()=>byId('node-detail').close());
    byId('alert-detail-close').addEventListener('click',()=>{alertDetailGeneration++;byId('alert-detail').close();});
    window.addEventListener('focus',expireHealth);
    document.addEventListener('visibilitychange',()=>{if(!document.hidden)expireHealth();});
    setView('current');refresh();setInterval(refresh,15000);
