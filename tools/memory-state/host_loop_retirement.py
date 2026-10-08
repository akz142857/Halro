"""Join only the actual, exclusive host backing after its namespace exits."""
import json
import pathlib
import re

BACKING = '/halro-host-owned/state.ext4.img'
PREPARE_MARKER = 'HALRO_EXT4_HOST_JOIN_PREPARED '
JOIN_MARKER = 'HALRO_HOST_LOOP_JOIN '


def prepared_join(runtime_log, state, image_bytes):
    # A stopped process is only a prerequisite, never a disk retirement grant.
    if (not isinstance(state, dict) or state.get('Running') is not False or
            type(state.get('Pid')) is not int or state['Pid'] != 0):
        raise ValueError('original container namespace not joined; host owner retained')
    entries = [json.loads(line[len(PREPARE_MARKER):]) for line in runtime_log.splitlines()
               if line.startswith(PREPARE_MARKER)]
    if len(entries) != 1:
        raise ValueError('missing unique original actual unmount/detach/reader receipt')
    receipt = entries[0]
    keys = {'backing', 'unmounted', 'detach_requested', 'reader_closed',
            'backing_device', 'backing_inode', 'backing_size', 'original_loop',
            'original_loop_device', 'namespace_join_pending'}
    if not isinstance(receipt, dict) or set(receipt) != keys:
        raise ValueError('invalid original join receipt')
    for field in ('unmounted', 'detach_requested', 'reader_closed', 'namespace_join_pending'):
        if receipt[field] is not True:
            raise ValueError('actual original unmount/detach/reader join is unknown')
    if (receipt['backing'] != BACKING or type(receipt['backing_size']) is not int or
            receipt['backing_size'] != image_bytes or
            type(receipt['backing_inode']) is not int or receipt['backing_inode'] <= 0 or
            not isinstance(receipt['backing_device'], str) or
            not re.fullmatch(r'[0-9]+:[0-9]+', receipt['backing_device']) or
            not isinstance(receipt['original_loop'], str) or
            not re.fullmatch(r'/dev/loop[0-9]+', receipt['original_loop']) or
            not isinstance(receipt['original_loop_device'], str) or
            not re.fullmatch(r'7:[0-9]+', receipt['original_loop_device']) or
            receipt['original_loop_device'] != '7:' + receipt['original_loop'][9:]):
        raise ValueError('original actual backing or loop identity is invalid')
    return receipt


def finish_host_join(host, state, runtime_log, docker, image_id, container, root):
    """Hold the original host creator through rm+independent actual loop join.

    docker captures every exit/stdout/stderr. Unknown rm, verifier IO, reader
    Close, inode or allocation keeps the complete host owner; no foreign loop
    is detached. The verifier selects by real backing dev/inode, never path.
    """
    receipt = prepared_join(runtime_log, state, host.size)
    host.verify()
    host.record['inner_join_prepared'] = receipt
    host.record['state'] = 'namespace_join_pending_owner_retained'
    host._save()
    # Removing only this known-stopped task container joins Docker's namespace
    # references. The original creator FD remains live throughout this step.
    docker('rm', container)
    host.record['original_container_removed'] = True
    host._save()
    helper = pathlib.Path(root) / 'tools/memory-state/ext4_host_join_linux.sh'
    result = docker('run', '--rm', '--pull=never', '--network=none', '--privileged',
        '--memory=2g', '--memory-swap=2g', '--cpus=2', '--pids-limit=64',
        '--mount', f'type=bind,src={host.path},dst={BACKING},readonly',
        '--mount', f'type=bind,src={helper},dst=/join.sh,readonly',
        image_id, '/bin/sh', '/join.sh', receipt['backing_device'],
        receipt['backing_inode'], receipt['backing_size'], receipt['original_loop'],
        receipt['original_loop_device'])
    lines = [json.loads(line[len(JOIN_MARKER):]) for line in result.stdout.splitlines()
             if line.startswith(JOIN_MARKER)]
    if result.returncode != 0 or len(lines) != 1:
        raise ValueError('independent actual backing/loop join unconfirmed; owner retained')
    proof = lines[0]
    expected = {key: receipt[key] for key in ('backing_device', 'backing_inode',
                'backing_size', 'original_loop', 'original_loop_device')}
    if (not isinstance(proof, dict) or
            set(proof) != set(expected) | {'loop_absent', 'observer_reader_closed', 'polls'} or
            any(proof[key] != value or type(proof[key]) is not type(value)
                for key, value in expected.items()) or
            proof['loop_absent'] is not True or proof['observer_reader_closed'] is not True or
            type(proof['polls']) is not int or not 0 <= proof['polls'] <= 100):
        raise ValueError('independent actual join identity or reader retirement mismatch')
    host.verify()
    host.record['independent_loop_join'] = proof
    host.record['independent_loop_absent_verified'] = True
    host._save()
    cleanup = {'backing': BACKING, 'unmounted': True, 'loop_absent': True,
               'autoclear_completed': True, 'reader_closed': True}
    host.retire(cleanup)
    return proof
