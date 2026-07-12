#!/bin/zsh
set -euo pipefail

python_bin="${IDLELOOM_TRAIN_PYTHON:-/var/tmp/idleloom-mlx/bin/python}"
if [[ ! -x "${python_bin}" ]]; then
  print -u2 "MLX Python was not found at ${python_bin}"
  exit 1
fi

PYTHONDONTWRITEBYTECODE=1 "${python_bin}" -c '
import mlx.core as mx

mx.set_default_device(mx.gpu)

x = mx.linspace(-2.0, 2.0, 4096)
y = 3.0 * x + 2.0
weight = mx.array(0.0)
bias = mx.array(0.0)


def loss_fn(current_weight, current_bias):
    prediction = current_weight * x + current_bias
    return mx.mean(mx.square(prediction - y))


loss_and_grad = mx.value_and_grad(loss_fn, argnums=(0, 1))
learning_rate = 0.08

for step in range(101):
    loss, (weight_grad, bias_grad) = loss_and_grad(weight, bias)
    weight = weight - learning_rate * weight_grad
    bias = bias - learning_rate * bias_grad
    mx.eval(weight, bias, loss)
    if step % 20 == 0:
        print(f"step={step:03d} loss={float(loss):.8f}")

mx.savez("checkpoint.npz", weight=weight, bias=bias)
print(f"device={mx.default_device()}")
print(f"weight={float(weight):.6f} bias={float(bias):.6f}")
print(f"metal_active_memory={mx.get_active_memory()}")
print("checkpoint=checkpoint.npz")
'

/usr/bin/shasum -a 256 checkpoint.npz
