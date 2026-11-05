#!/usr/bin/env node
// Synthetic-only clock-pricing recovery against the production bundle.
import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { installConsoleFixture } from './console-acceptance-fixtures.mjs';
import { zhCN } from '../src/i18n/locales/zh-CN.ts';
import { enUS } from '../src/i18n/locales/en-US.ts';
const { chromium } = await import(pathToFileURL(resolve(process.env.PLAYWRIGHT_MODULE)).href);
const baseURL=process.env.BASE_URL??'http://127.0.0.1:5277/admin/';
const outputDir=resolve(process.env.OUTPUT_DIR??'/private/tmp/halro-clock-pricing-browser');
await mkdir(outputDir,{recursive:true});
const browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_EXECUTABLE,args:['--disable-background-networking','--disable-component-update','--disable-sync']});
const results=[];
try{
 for(const locale of ['zh-CN','en-US'])for(const width of [1440,390])for(const readOnly of [false,true]){
  const name=`${locale}-${width}-${readOnly?'read-only':'recover'}`;const t=locale==='zh-CN'?zhCN:enUS;
  const context=await browser.newContext({viewport:{width,height:900},locale});const page=await context.newPage();const faults=[];page.on('pageerror',e=>faults.push(e.message));
  let attempts=0;let reviews=0;let revision=3;
  const state=await installConsoleFixture(page,{baseURL,locale,role:readOnly?'read_only':'administrator',respond:(state,method,path,_q,body)=>{
   if(path==='/session')state.deployments[0].pricing_quarantined=true,state.deployments[0].pricing_quarantine_reason='wall_clock_forward_jump';
   if(path.endsWith('/prices/clock-confirm')){
    if(method==='GET'){reviews++;return {status:200,data:{deployment_id:state.deployments[0].id,confirmation:{revision,reason:'wall_clock_forward_jump',price_id:state.prices[0].id,reviewed_at:new Date().toISOString()},previous_selected_at:'2026-11-04T12:00:00Z',price:state.prices[0]}};}
    attempts++;
    if(attempts===1){revision++;return {status:412,data:{error:'review changed',code:'revision_mismatch'}};}
    assert.equal(body.confirmation.revision,revision);assert.equal(body.confirmation.price_id,state.prices[0].id);assert.equal(body.current_password,'synthetic-password');
    state.deployments[0].pricing_quarantined=false;state.deployments[0].pricing_quarantine_reason='';return {status:200,data:{deployment_id:state.deployments[0].id,pricing_quarantine:false}};
   }
  }});
  try{
   await page.goto(new URL('/admin/deployments',baseURL).href,{waitUntil:'networkidle'});
   await page.locator('button[aria-haspopup="dialog"]').first().click();
   const open=page.getByRole('button',{name:t.deployments.confirmClockPricing,exact:true});await open.waitFor();
   if(readOnly){assert.equal(await open.isDisabled(),true);assert.equal(reviews,0);}else{
    await open.click();const dialog=page.getByRole('dialog',{name:t.deployments.confirmClockPricing,exact:true});await dialog.waitFor();
    const submit=dialog.getByRole('button',{name:t.deployments.confirmClockPricing,exact:true});const check=dialog.getByRole('checkbox');
    await check.waitFor();assert.equal(await submit.isDisabled(),true);
    await dialog.locator('input[type="password"]').fill('synthetic-password');assert.equal(await submit.isDisabled(),true);
    await check.check();await submit.click();await dialog.getByRole('alert').waitFor();assert.equal(attempts,1);
    await dialog.getByRole('button',{name:t.deployments.clockRefreshReview,exact:true}).click();
    await page.waitForFunction(()=>!document.querySelector('[role="dialog"] input[type="checkbox"]:checked'));
    assert.equal(await submit.isDisabled(),true);await check.check();
    assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);
    await page.screenshot({path:join(outputDir,`${name}.png`),fullPage:true});
    await submit.click();await dialog.waitFor({state:'hidden'});assert.equal(attempts,2);assert.equal(reviews,2);
   }
   assert.deepEqual(faults,[]);assert.deepEqual(state.unknownRoutes,[]);assert.deepEqual(state.blockedRequests,[]);
   results.push({name,passed:true});
  }catch(error){results.push({name,passed:false,error:error.message});}
  await context.close();process.stdout.write(`${results.at(-1).passed?'PASS':'FAIL'} ${name}${results.at(-1).error?': '+results.at(-1).error:''}\n`);
 }
}finally{await browser.close();await writeFile(join(outputDir,'report.json'),JSON.stringify({syntheticOnly:true,results},null,2));if(results.some(v=>!v.passed))process.exitCode=1;}
