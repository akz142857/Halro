import hashlib
import json
import os
import pathlib
import subprocess
import tempfile
import time
import unittest

from host_backing import HostBacking
from host_loop_retirement import finish_host_join, prepared_join

ROOT = pathlib.Path(__file__).resolve().parents[2]


class Ext4HostFixtureIntegrationTest(unittest.TestCase):
    @unittest.skipUnless(os.environ.get('HALRO_HOST_BACKING_INTEGRATION') == '1',
                         'explicit 512MiB own-host Docker fixture opt-in required')
    def test_actual_512mib_preallocation_dio_ext4_fresh_io_and_retirement(self):
        out = pathlib.Path(tempfile.mkdtemp(prefix='halro-host-backing-512-evidence-', dir='/tmp'))
        input_names = ('host_backing.py', 'host_loop_retirement.py', 'ext4_host_fixture_linux.sh',
                       'ext4_host_join_linux.sh', 'test_ext4_host_fixture.py')
        def inputs():
            return {name: hashlib.sha256(pathlib.Path(__file__).with_name(name).read_bytes()).hexdigest()
                    for name in input_names}
        before = inputs()
        (out / 'source-before.json').write_text(json.dumps(before, indent=2) + '\n')
        records = []
        def docker(*args):
            argv = ['docker', *map(str, args)]
            r = subprocess.run(argv, text=True, capture_output=True, timeout=180)
            records.append({'argv': argv, 'code': r.returncode, 'stdout': r.stdout, 'stderr': r.stderr})
            (out / 'commands.json').write_text(json.dumps(records, indent=2) + '\n')
            if r.returncode:
                raise RuntimeError(r.stderr or r.stdout or 'Docker command failed')
            return r
        def stopped(name):
            for _ in range(100):
                state = json.loads(docker('inspect', name).stdout)[0]['State']
                if state['Running'] is False:
                    return state
                time.sleep(0.05)
            raise RuntimeError('owned fixture process not joined')
        runner = pathlib.Path(__file__).with_name('ext4_host_fixture_linux.sh').resolve()
        image = json.loads(docker('image', 'inspect', 'ubuntu:24.04').stdout)[0]['Id']
        host = HostBacking('/tmp', 512, out / 'host-backing.json')
        # A second actual creator gives the same guest path to a DIFFERENT inode.
        # It stays live throughout the original's independent loop retirement.
        foreign = HostBacking('/tmp', 128, out / 'foreign-host-backing.json')
        suffix = out.name.rsplit('-', 1)[1]
        primary_name = 'halro-host-backing-512-' + suffix
        foreign_name = 'halro-host-backing-foreign-' + suffix
        primary_started = foreign_started = False
        result = {'result': 'failed', 'image_id': image,
                  'runner_sha256': hashlib.sha256(runner.read_bytes()).hexdigest(),
                  'scope': 'actual512MiB ext4 + two fresh shell IO processes + live128MiB foreign-loop isolation; not Runtime capacity evidence'}
        def launch(name, owner, mib, script):
            return docker('run', '--detach', '--name', name, '--pull=never', '--privileged',
                '--network=none', '--memory=2g', '--memory-swap=2g', '--cpus=2', '--pids-limit=64',
                '-e', f'HALRO_EXT4_IMAGE_MIB={mib}',
                '--mount', f'type=bind,src={owner.path},dst=/halro-host-owned/state.ext4.img',
                '--mount', f'type=bind,src={runner},dst=/runner.sh,readonly',
                image, '/bin/sh', '/runner.sh', '/bin/sh', '-c', script)
        try:
            launch(foreign_name, foreign, 128, '''set -eu
                printf 'FOREIGN_LOOP_READY\\n'
                n=0
                while test ! -e "$HALRO_EXT4_ROOT/stop" && test "$n" -lt 600; do
                  sleep 0.05; n=$((n+1))
                done
                test -e "$HALRO_EXT4_ROOT/stop"
                ''')
            foreign_started = True
            for _ in range(100):
                if 'FOREIGN_LOOP_READY' in docker('logs', foreign_name).stdout:
                    break
                time.sleep(0.05)
            else:
                raise RuntimeError('actual foreign loop did not become ready')
            foreign_before = docker('exec', foreign_name, '/bin/sh', '-c',
                'set -eu; own=$(findmnt -n -o SOURCE -M /halro-fixture/data); '
                'losetup -n --raw -O NAME,BACK-INO,BACK-MAJ:MIN,MAJ:MIN,DIO "$own"').stdout
            launch(primary_name, host, 512, '''set -eu
                printf 'CGROUP_BEFORE max='; cat /sys/fs/cgroup/memory.max
                cat /sys/fs/cgroup/memory.current /sys/fs/cgroup/memory.peak /sys/fs/cgroup/memory.events
                test "$(cat /sys/fs/cgroup/memory.max)" = 2147483648
                /bin/sh -c 'set -eu; echo "SEED_PROCESS $$"; dd if=/dev/urandom of="$HALRO_EXT4_ROOT/facts" bs=4096 count=8 status=none; sync; sha256sum "$HALRO_EXT4_ROOT/facts" > "$HALRO_EXT4_ROOT/facts.sha"'
                /bin/sh -c 'set -eu; echo "FRESH_RESTART_PROCESS $$"; sha256sum -c "$HALRO_EXT4_ROOT/facts.sha"'
                owned_loop=$(findmnt -n -o SOURCE -M /halro-fixture/data)
                /bin/sh -c 'exec 7< "$1"; sleep 0.2; exec 7<&-' borrow "$owned_loop" &
                sleep 0.05
                printf 'CGROUP_AFTER '; cat /sys/fs/cgroup/memory.current /sys/fs/cgroup/memory.peak /sys/fs/cgroup/memory.events
                ''')
            primary_started = True
            state = stopped(primary_name)
            log = docker('logs', primary_name)
            runtime = log.stdout + log.stderr
            (out / 'runtime.log').write_text(runtime)
            (out / 'state.json').write_text(json.dumps(state, indent=2) + '\n')
            self.assertEqual(state['ExitCode'], 0, runtime)
            self.assertFalse(state['OOMKilled'])
            self.assertIn('HALRO_EXT4_LOOP direct_io=1 owned_backing=/halro-host-owned/state.ext4.img', runtime)
            original = prepared_join(runtime, state, host.size)
            foreign_fields = foreign_before.strip().split()
            self.assertNotEqual(str(original['backing_inode']), foreign_fields[1])
            self.assertEqual(original['backing_device'], foreign_fields[2])
            self.assertEqual(foreign_fields[4], '1')
            finish_host_join(host, state, runtime, docker, image, primary_name, ROOT)
            self.assertEqual(host.record['state'], 'retired_after_actual_loop_cleanup')
            self.assertFalse(host.path.exists())
            # The original's proof must neither stop nor detach this live,
            # different-inode loop with the exact same BACK-FILE string.
            foreign_after = docker('exec', foreign_name, '/bin/sh', '-c',
                'set -eu; own=$(findmnt -n -o SOURCE -M /halro-fixture/data); '
                'losetup -n --raw -O NAME,BACK-INO,BACK-MAJ:MIN,MAJ:MIN,DIO "$own"').stdout
            self.assertEqual(foreign_after, foreign_before)
            foreign.verify()
            (out / 'foreign-isolation.json').write_text(json.dumps({
                'original_receipt': original, 'foreign_before': foreign_before,
                'foreign_after': foreign_after, 'foreign_original_fd_live': True}, indent=2) + '\n')
            docker('exec', foreign_name, '/bin/sh', '-c', 'touch /halro-fixture/data/stop')
            foreign_state = stopped(foreign_name)
            foreign_log = docker('logs', foreign_name)
            foreign_runtime = foreign_log.stdout + foreign_log.stderr
            (out / 'foreign-runtime.log').write_text(foreign_runtime)
            self.assertEqual(foreign_state['ExitCode'], 0, foreign_runtime)
            self.assertFalse(foreign_state['OOMKilled'])
            finish_host_join(foreign, foreign_state, foreign_runtime, docker, image, foreign_name, ROOT)
            result['result'] = 'passed'
        except BaseException as error:
            result['error'] = str(error)
            for owner in (host, foreign):
                if owner.fd is not None:
                    owner.record['state'] = 'cleanup_unconfirmed_retained'
                    owner._save()
        finally:
            (out / 'verification.json').write_text(json.dumps(result, indent=2) + '\n')
            # This cancels only our disposable jobs; it never grants file cleanup.
            for name, started, owner in ((primary_name, primary_started, host),
                                          (foreign_name, foreign_started, foreign)):
                if started and owner.fd is not None:
                    try:
                        actual = json.loads(docker('inspect', name).stdout)[0]['State']
                        if actual['Running'] and name == foreign_name:
                            docker('exec', name, '/bin/sh', '-c', 'touch /halro-fixture/data/stop')
                            actual = stopped(name)
                        if actual['Running'] is False:
                            final_log = docker('logs', name)
                            (out / (name + '-final.log')).write_text(final_log.stdout + final_log.stderr)
                            finish_host_join(owner, actual, final_log.stdout + final_log.stderr,
                                             docker, image, name, ROOT)
                    except BaseException as error:
                        result.setdefault('cleanup_errors', []).append(str(error))
                        owner.record['state'] = 'cleanup_unconfirmed_retained'
                        owner._save()
                if started:
                    try:
                        docker('rm', '-f', name)
                    except RuntimeError:
                        pass
            (out / 'verification.json').write_text(json.dumps(result, indent=2) + '\n')
        after = inputs()
        (out / 'source-after.json').write_text(json.dumps(after, indent=2) + '\n')
        result['source_unchanged'] = before == after
        if before != after:
            result.update(result='failed', error='actual fixture source changed during execution')
        (out / 'verification.json').write_text(json.dumps(result, indent=2) + '\n')
        print(out)
        print(json.dumps(result))
        self.assertEqual(result['result'], 'passed', 'Original fixture evidence retained at ' + str(out))


if __name__ == '__main__':
    unittest.main()
