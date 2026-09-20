"""pybase.playwright_launch 的单元测试。"""
from __future__ import annotations

from pybase.playwright_launch import playwright_context_args, playwright_launch_args


def test_launch_args_defaults_headless_false():
    assert playwright_launch_args({}) == {"headless": False}


def test_launch_args_headless_true():
    assert playwright_launch_args({"headless": True}) == {"headless": True}


def test_launch_args_with_channel():
    args = playwright_launch_args({"headless": True, "channel": "chrome"})
    assert args == {"headless": True, "channel": "chrome"}


def test_launch_args_with_extra_args_list():
    args = playwright_launch_args({"args": ["--no-sandbox", "--disable-gpu"]})
    assert args == {"headless": False, "args": ["--no-sandbox", "--disable-gpu"]}


def test_launch_args_omits_empty_channel_and_args():
    args = playwright_launch_args({"channel": "", "args": []})
    assert args == {"headless": False}


def test_context_args_maps_viewport():
    assert playwright_context_args({"width": 1440, "height": 900}) == {
        "viewport": {"width": 1440, "height": 900}
    }


def test_context_args_coerces_to_int():
    assert playwright_context_args({"width": "800", "height": "600"}) == {
        "viewport": {"width": 800, "height": 600}
    }
